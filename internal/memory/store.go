// Package memory owns tenant memory, session history, and their shared storage lifecycle.
package memory

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

// Store manages speaker profiles, user memories, and session memory in SQLite.
type Store struct {
	db             *database.DB
	sql            *sql.DB
	log            *config.Logger
	embedder       llm.Embedder
	embedModel     string
	indexNotify    func()
	retention      config.RetentionPolicy
	mutationMu     sync.Mutex
	lastOptimizeAt time.Time
	userLocks      map[string]*sync.Mutex

	formationFailpoint func(string) error
	indexWriteHook     func(string)
}

// NewSQLiteStore opens or initializes a supported SQLite-backed memory store.
func NewSQLiteStore(dbPath string, embedder llm.Embedder, embeddingModel string, log *config.Logger) (*Store, error) {
	db, err := database.Open(dbPath, log)
	if err != nil {
		return nil, err
	}
	return &Store{
		db:         db,
		sql:        db.SQL(),
		log:        log,
		embedder:   embedder,
		embedModel: strings.TrimSpace(embeddingModel),
	}, nil
}

// Close closes the store database connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// SetDerivedIndexNotifier installs a nonblocking wake-up callback for the
// durable derived-index worker. Correctness never depends on the callback.
func (s *Store) SetDerivedIndexNotifier(notify func()) {
	s.indexNotify = notify
}

// SetRetentionPolicy applies configured lifecycle durations to session writes.
// Tests and embedders that do not call it retain their explicitly supplied TTLs.
func (s *Store) SetRetentionPolicy(policy config.RetentionPolicy) {
	s.retention = policy
}

func (s *Store) sessionTTL(fallback time.Duration) time.Duration {
	if s.retention.SessionInactivity > 0 {
		return s.retention.SessionInactivity
	}
	if fallback > 0 {
		return fallback
	}
	return 24 * time.Hour
}

func (s *Store) signalDerivedIndex() {
	if s.indexNotify != nil {
		s.indexNotify()
	}
}

func (s *Store) lockUsers(userIDs ...string) func() {
	ids := uniqueStrings(userIDs)
	sort.Strings(ids)
	s.mutationMu.Lock()
	if s.userLocks == nil {
		s.userLocks = make(map[string]*sync.Mutex)
	}
	locks := make([]*sync.Mutex, 0, len(ids))
	for _, userID := range ids {
		lock := s.userLocks[userID]
		if lock == nil {
			lock = &sync.Mutex{}
			s.userLocks[userID] = lock
		}
		locks = append(locks, lock)
	}
	s.mutationMu.Unlock()
	for _, lock := range locks {
		lock.Lock()
	}
	return func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}
}

func (s *Store) ensureAccountUser(userID string) error {
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("user memory: user id is required")
	}
	var exists int
	err := s.sql.QueryRow(`SELECT 1 FROM account_users WHERE canonical_user_id = ?`, userID).Scan(&exists)
	if err == sql.ErrNoRows {
		return fmt.Errorf("user memory: account user %q does not exist", userID)
	}
	if err != nil {
		return fmt.Errorf("failed to check account user %q: %w", userID, err)
	}
	return nil
}

func nullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return formatTime(*t)
}

func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	return t
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
