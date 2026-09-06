package broker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

const requestQueueSize = 10

var (
	// ErrQueueFull indicates that the broker has reached its global work limit.
	ErrQueueFull = errors.New("broker queue full")
	// ErrShuttingDown indicates that the broker no longer accepts new work.
	ErrShuttingDown = errors.New("broker is shutting down")
	// ErrAgentWorkCanceled indicates that a stop command canceled foreground agent work.
	ErrAgentWorkCanceled = errors.New("agent work canceled")
)

// CancelReport describes foreground agent work affected by a cancellation request.
type CancelReport struct {
	ActiveSignaled int
	QueuedCanceled int
}

// LaneKey identifies work that must execute serially and in acceptance order.
type LaneKey struct {
	CanonicalUserID string
	SessionID       string
	UserExclusive   bool
}

// Request carries a single user request from a gateway into the broker.
type Request struct {
	RequestID        string
	ChatID           string
	Principal        identity.Principal
	DisplayName      string
	SessionKey       string
	IsDirect         bool
	Prompt           string
	Images           []llm.InputImage
	StreamFunc       func(agent.StreamChunk)
	RefreshPrincipal func(identity.Principal) (identity.Principal, error)
	ResponseChan     chan Result
}

// Result is the response payload delivered to the originating gateway.
type Result struct {
	Response  *agent.Response
	Principal identity.Principal
	Err       error
}

type work struct {
	key             LaneKey
	run             func() error
	finish          func(error)
	request         *Request
	fences          []*userFence
	reservedReaders []bool
	releasedFences  []bool
	exclusive       bool
	gated           bool
	ctx             context.Context
	cancel          context.CancelCauseFunc
	ownerUserID     string
	state           workState
	deliverOnce     sync.Once
}

type workState uint8

const (
	workQueued workState = iota
	workRunning
	workCanceled
	workCompleted
)

type userFence struct {
	mu             sync.Mutex
	cond           *sync.Cond
	readers        int
	writer         bool
	pendingWriters int
}

func newUserFence() *userFence {
	fence := &userFence{}
	fence.cond = sync.NewCond(&fence.mu)
	return fence
}

func (f *userFence) reserveWriter() {
	f.mu.Lock()
	f.pendingWriters++
	f.mu.Unlock()
}

func (f *userFence) reserveReader() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writer || f.pendingWriters > 0 {
		return false
	}
	f.readers++
	return true
}

func (f *userFence) acquire(exclusive, reservedReader bool) {
	if reservedReader {
		return
	}
	f.mu.Lock()
	if exclusive {
		for f.writer || f.readers > 0 {
			f.cond.Wait()
		}
		f.pendingWriters--
		f.writer = true
	} else {
		for f.writer || f.pendingWriters > 0 {
			f.cond.Wait()
		}
		f.readers++
	}
	f.mu.Unlock()
}

func (f *userFence) release(exclusive bool) {
	f.mu.Lock()
	if exclusive {
		f.writer = false
	} else {
		f.readers--
	}
	f.cond.Broadcast()
	f.mu.Unlock()
}

// Broker schedules FIFO work per canonical-user/session lane while allowing
// unrelated lanes to use the worker pool concurrently.
type Broker struct {
	agent       Processor
	workerCount int
	ready       chan *work
	log         *config.Logger

	mu              sync.Mutex
	lanes           map[LaneKey][]*work
	userFences      map[string]*userFence
	outstanding     int
	accepting       bool
	started         bool
	background      *backgroundPermit
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc

	workWG    sync.WaitGroup
	workerWG  sync.WaitGroup
	startOnce sync.Once
	shutdown  sync.Once
}

type backgroundPermit struct {
	cancel context.CancelFunc
}

// Processor handles one typed agent request.
type Processor interface {
	Process(context.Context, agent.Request) (*agent.Response, error)
}

// NewBroker creates a lane-aware broker. Call Start before production use.
func NewBroker(aiAgent Processor, workerCount int, log *config.Logger) *Broker {
	if workerCount <= 0 {
		workerCount = 1
	}
	capacity := requestQueueSize + workerCount
	if capacity < requestQueueSize {
		capacity = requestQueueSize
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	return &Broker{
		agent:           aiAgent,
		workerCount:     workerCount,
		ready:           make(chan *work, capacity),
		log:             log,
		lanes:           make(map[LaneKey][]*work),
		userFences:      make(map[string]*userFence),
		accepting:       true,
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
	}
}

// Start launches the worker pool once.
func (b *Broker) Start() {
	b.startOnce.Do(func() {
		b.mu.Lock()
		b.started = true
		var heads []*work
		for _, lane := range b.lanes {
			if len(lane) > 0 {
				heads = append(heads, lane[0])
			}
		}
		b.mu.Unlock()
		for i := 0; i < b.workerCount; i++ {
			b.workerWG.Add(1)
			go b.runWorker(i + 1)
		}
		for _, head := range heads {
			b.makeReady(head)
		}
		b.log.Info("broker.started", "started broker worker pool", config.F("worker_count", b.workerCount))
	})
}

// Submit enqueues an agent request. Rejected requests receive one immediate result.
func (b *Broker) Submit(req *Request) error {
	if req.ResponseChan == nil {
		req.ResponseChan = make(chan Result, 1)
	}
	requestCtx, cancel := context.WithCancelCause(b.lifecycleCtx)
	w := &work{key: laneKey(req.Principal, req.SessionKey), request: req, ctx: requestCtx, cancel: cancel, ownerUserID: req.Principal.CanonicalUserID}
	deliver := func(result Result) {
		w.deliverOnce.Do(func() { deliverResult(req.ResponseChan, result) })
	}
	w.run = func() error {
		if cause := context.Cause(w.ctx); cause != nil {
			deliver(Result{Err: cause})
			return nil
		}
		if req.RefreshPrincipal != nil {
			resolved := false
			for attempt := 0; attempt < 8; attempt++ {
				previousUserID := req.Principal.CanonicalUserID
				principal, err := req.RefreshPrincipal(req.Principal)
				if err != nil {
					deliver(Result{Principal: req.Principal, Err: err})
					return nil
				}
				req.Principal = principal
				b.mu.Lock()
				w.ownerUserID = principal.CanonicalUserID
				b.mu.Unlock()
				if principal.CanonicalUserID == previousUserID {
					resolved = true
					break
				}
				b.transferReaderFence(w, principal.CanonicalUserID)
			}
			if !resolved {
				err := fmt.Errorf("principal ownership changed too many times while queued")
				deliver(Result{Principal: req.Principal, Err: err})
				return nil
			}
		}
		if cause := context.Cause(w.ctx); cause != nil {
			deliver(Result{Principal: req.Principal, Err: cause})
			return nil
		}
		resp, err := b.agent.Process(w.ctx, agent.Request{
			RequestID: req.RequestID, Principal: req.Principal, DisplayName: req.DisplayName,
			SessionKey: req.SessionKey, Prompt: req.Prompt, Images: req.Images, StreamFunc: req.StreamFunc,
			IsDirect: req.IsDirect,
		})
		if cause := context.Cause(w.ctx); cause != nil {
			err = cause
			resp = nil
		}
		deliver(Result{Response: resp, Principal: req.Principal, Err: err})
		return nil
	}
	w.finish = func(err error) {
		if err != nil {
			deliver(Result{Err: err})
		}
	}
	b.log.Debug("broker.request.queued", "queued broker request",
		config.F("request_id", req.RequestID), config.F("gateway", req.Principal.Gateway),
		config.F("chat_id", req.ChatID), config.F("session_id", req.SessionKey))
	if err := b.enqueue(w); err != nil {
		cancel(err)
		reason := "queue_full"
		text := "request rejected: broker queue full"
		if errors.Is(err, ErrShuttingDown) {
			reason = "shutting_down"
			text = "request rejected: broker shutting down"
		}
		b.log.Warn("broker.request.rejected", "rejected broker request",
			config.F("request_id", req.RequestID), config.F("gateway", req.Principal.Gateway),
			config.F("chat_id", req.ChatID), config.F("status", "rejected"), config.F("reason", reason))
		deliverResult(req.ResponseChan, Result{Response: &agent.Response{Response: config.SafeText(text)}})
		return err
	}
	return nil
}

// CancelActiveAgentWork cancels the currently running foreground request in one conversation.
func (b *Broker) CancelActiveAgentWork(canonicalUserID, sessionID string) CancelReport {
	canonicalUserID = strings.TrimSpace(canonicalUserID)
	sessionID = strings.TrimSpace(sessionID)
	if canonicalUserID == "" || sessionID == "" {
		return CancelReport{}
	}
	return b.cancelAgentWork(func(w *work) bool {
		return w.state == workRunning && w.ownerUserID == canonicalUserID && w.key.SessionID == sessionID
	})
}

// CancelAllAgentWork cancels all active and queued foreground agent requests.
func (b *Broker) CancelAllAgentWork() CancelReport {
	return b.cancelAgentWork(func(w *work) bool {
		return w.state == workRunning || w.state == workQueued
	})
}

func (b *Broker) cancelAgentWork(matches func(*work) bool) CancelReport {
	b.mu.Lock()
	var report CancelReport
	var canceled []*work
	for _, lane := range b.lanes {
		for _, w := range lane {
			if w.request == nil || !matches(w) {
				continue
			}
			if w.state == workRunning {
				report.ActiveSignaled++
			} else {
				report.QueuedCanceled++
			}
			w.state = workCanceled
			w.cancel(ErrAgentWorkCanceled)
			canceled = append(canceled, w)
		}
	}
	b.mu.Unlock()
	for _, w := range canceled {
		w.deliverOnce.Do(func() { deliverResult(w.request.ResponseChan, Result{Err: ErrAgentWorkCanceled}) })
	}
	return report
}

// RunInLane runs a synchronous gateway operation in the same FIFO lane used by agent work.
func (b *Broker) RunInLane(ctx context.Context, principal identity.Principal, sessionID string, operation func() error) error {
	return b.runOperationForUsers(ctx, LaneKey{CanonicalUserID: principal.CanonicalUserID, SessionID: sessionID}, []string{principal.CanonicalUserID}, operation)
}

// RunUsersExclusive waits for active work for every listed user and blocks new
// work for those users until completion. IDs are deduplicated and locked in a
// stable order so overlapping multi-user operations cannot deadlock.
func (b *Broker) RunUsersExclusive(ctx context.Context, canonicalUserIDs []string, operation func() error) error {
	userIDs := normalizeUserIDs(canonicalUserIDs)
	if len(userIDs) == 0 {
		return fmt.Errorf("broker exclusive operation requires a canonical user ID")
	}
	return b.runOperationForUsers(ctx, LaneKey{CanonicalUserID: userIDs[0], UserExclusive: true}, userIDs, operation)
}

func (b *Broker) runOperationForUsers(ctx context.Context, key LaneKey, userIDs []string, operation func() error) error {
	if operation == nil {
		return fmt.Errorf("broker lane operation is required")
	}
	done := make(chan error, 1)
	w := &work{key: key, run: operation, finish: func(err error) { done <- err }, exclusive: key.UserExclusive}
	w.fences, w.reservedReaders = b.reserveFences(userIDs, w.exclusive)
	if err := b.enqueue(w); err != nil {
		b.cancelFenceReservations(w)
		return err
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Broker) enqueue(w *work) error {
	b.mu.Lock()
	if !b.accepting {
		b.mu.Unlock()
		return ErrShuttingDown
	}
	limit := requestQueueSize + b.workerCount
	if b.outstanding >= limit {
		b.mu.Unlock()
		return ErrQueueFull
	}
	if b.background != nil {
		b.background.cancel()
	}
	lane := b.lanes[w.key]
	if len(w.fences) == 0 {
		fence := b.userFences[w.key.CanonicalUserID]
		if fence == nil {
			fence = newUserFence()
			b.userFences[w.key.CanonicalUserID] = fence
		}
		w.fences = []*userFence{fence}
		if w.exclusive {
			fence.reserveWriter()
		} else {
			w.reservedReaders = []bool{fence.reserveReader()}
		}
	}
	b.lanes[w.key] = append(lane, w)
	b.outstanding++
	b.workWG.Add(1)
	isHead := len(lane) == 0
	if isHead {
		if b.started {
			b.makeReady(w)
		}
	}
	b.mu.Unlock()
	return nil
}

// TryAcquireLowPriority reserves model capacity only while no foreground work
// is active or queued. Accepted foreground work cancels the returned context.
func (b *Broker) TryAcquireLowPriority(parent context.Context) (context.Context, func(), bool) {
	b.mu.Lock()
	if !b.accepting || !b.started || b.outstanding != 0 || b.background != nil {
		b.mu.Unlock()
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	permit := &backgroundPermit{cancel: cancel}
	b.background = permit
	b.mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			cancel()
			b.mu.Lock()
			if b.background == permit {
				b.background = nil
			}
			b.mu.Unlock()
		})
	}
	return ctx, release, true
}

// Shutdown rejects new work, cancels active agent requests, drains accepted lanes, and stops workers.
func (b *Broker) Shutdown() {
	b.shutdown.Do(func() {
		b.mu.Lock()
		b.accepting = false
		b.lifecycleCancel()
		if b.background != nil {
			b.background.cancel()
		}
		queued := b.outstanding
		started := b.started
		b.mu.Unlock()
		b.log.Info("broker.shutdown.start", "shutting down broker", config.F("queued_request_count", queued))
		if started && b.workerCount > 0 {
			b.workWG.Wait()
		} else {
			b.rejectPendingWork()
		}
		close(b.ready)
		b.workerWG.Wait()
		b.log.Info("broker.shutdown.complete", "broker shutdown complete")
	})
}

func (b *Broker) rejectPendingWork() {
	b.mu.Lock()
	var pending []*work
	for _, lane := range b.lanes {
		pending = append(pending, lane...)
	}
	b.lanes = make(map[LaneKey][]*work)
	b.outstanding = 0
	b.mu.Unlock()
	for _, w := range pending {
		b.cancelFenceReservations(w)
		w.finish(ErrShuttingDown)
		b.workWG.Done()
	}
}

func (b *Broker) runWorker(id int) {
	defer b.workerWG.Done()
	b.log.Debug("broker.worker.started", "broker worker started", config.F("worker_id", id))
	for w := range b.ready {
		b.mu.Lock()
		canceled := w.state == workCanceled
		if !canceled {
			w.state = workRunning
		}
		b.mu.Unlock()
		if w.request != nil {
			b.log.Debug("broker.worker.processing", "broker worker processing request",
				config.F("worker_id", id), config.F("request_id", w.request.RequestID),
				config.F("gateway", w.request.Principal.Gateway), config.F("chat_id", w.request.ChatID))
		}
		var err error
		if canceled {
			err = ErrAgentWorkCanceled
		} else {
			err = safeRun(w.run)
		}
		w.finish(err)
		if w.gated {
			for i := len(w.fences) - 1; i >= 0; i-- {
				if i >= len(w.releasedFences) || !w.releasedFences[i] {
					w.fences[i].release(w.exclusive)
				}
			}
		}
		b.complete(w)
	}
	b.log.Debug("broker.worker.stopped", "broker worker stopped", config.F("worker_id", id))
}

func (b *Broker) transferReaderFence(w *work, canonicalUserID string) {
	for i, fence := range w.fences {
		if i >= len(w.releasedFences) {
			w.releasedFences = append(w.releasedFences, make([]bool, i-len(w.releasedFences)+1)...)
		}
		if !w.releasedFences[i] {
			fence.release(false)
			w.releasedFences[i] = true
		}
	}
	b.mu.Lock()
	fence := b.userFences[canonicalUserID]
	if fence == nil {
		fence = newUserFence()
		b.userFences[canonicalUserID] = fence
	}
	b.mu.Unlock()
	reserved := fence.reserveReader()
	if !reserved {
		fence.acquire(false, false)
	}
	w.fences = append(w.fences, fence)
	w.reservedReaders = append(w.reservedReaders, reserved)
	w.releasedFences = append(w.releasedFences, false)
}

func (b *Broker) complete(completed *work) {
	b.mu.Lock()
	completed.state = workCompleted
	lane := b.lanes[completed.key]
	if len(lane) > 0 {
		lane = lane[1:]
	}
	b.outstanding--
	var next *work
	if len(lane) == 0 {
		delete(b.lanes, completed.key)
	} else {
		b.lanes[completed.key] = lane
		next = lane[0]
	}
	b.mu.Unlock()
	b.workWG.Done()
	if next != nil {
		b.makeReady(next)
	}
}

func (b *Broker) makeReady(w *work) {
	go func() {
		for i, fence := range w.fences {
			reservedReader := i < len(w.reservedReaders) && w.reservedReaders[i]
			fence.acquire(w.exclusive, reservedReader)
		}
		w.gated = true
		b.ready <- w
	}()
}

func (b *Broker) reserveFences(userIDs []string, exclusive bool) ([]*userFence, []bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fences := make([]*userFence, 0, len(userIDs))
	reservedReaders := make([]bool, 0, len(userIDs))
	for _, userID := range userIDs {
		fence := b.userFences[userID]
		if fence == nil {
			fence = newUserFence()
			b.userFences[userID] = fence
		}
		if exclusive {
			fence.reserveWriter()
			reservedReaders = append(reservedReaders, false)
		} else {
			reservedReaders = append(reservedReaders, fence.reserveReader())
		}
		fences = append(fences, fence)
	}
	return fences, reservedReaders
}

func (b *Broker) cancelFenceReservations(w *work) {
	if !w.exclusive {
		for i, reserved := range w.reservedReaders {
			if reserved && i < len(w.fences) {
				w.fences[i].release(false)
			}
		}
		return
	}
	for _, fence := range w.fences {
		fence.mu.Lock()
		fence.pendingWriters--
		fence.cond.Broadcast()
		fence.mu.Unlock()
	}
}

func normalizeUserIDs(userIDs []string) []string {
	seen := make(map[string]struct{}, len(userIDs))
	normalized := make([]string, 0, len(userIDs))
	for _, userID := range userIDs {
		userID = strings.TrimSpace(userID)
		if userID == "" {
			continue
		}
		if _, ok := seen[userID]; ok {
			continue
		}
		seen[userID] = struct{}{}
		normalized = append(normalized, userID)
	}
	sort.Strings(normalized)
	return normalized
}

func safeRun(run func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("broker operation panicked: %v", recovered)
		}
	}()
	return run()
}

func deliverResult(ch chan Result, result Result) (delivered bool) {
	defer func() {
		if recover() != nil {
			delivered = false
		}
	}()
	select {
	case ch <- result:
		return true
	default:
		return false
	}
}

func laneKey(principal identity.Principal, sessionID string) LaneKey {
	return LaneKey{CanonicalUserID: principal.CanonicalUserID, SessionID: sessionID}
}
