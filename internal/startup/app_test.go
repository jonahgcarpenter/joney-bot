package startup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/global"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestError(t *testing.T) {
	cause := errors.New("original failure")
	for _, tc := range []struct {
		cause error
		want  string
	}{{nil, "initialization failed"}, {cause, "initialization failed: original failure"}} {
		err := &Error{Event: "app.test.failed", Message: "initialization failed", Cause: tc.cause}
		if err.Error() != tc.want || err.Event != "app.test.failed" || err.Message != "initialization failed" {
			t.Fatalf("unexpected error: %#v (%s)", err, err)
		}
		if errors.Unwrap(err) != tc.cause || (tc.cause != nil && !errors.Is(err, cause)) {
			t.Fatalf("lost cause: %v", err)
		}
	}
}

func TestRunPreCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Nil inputs prove cancellation precedes configuration, logging, and storage.
	if err := Run(ctx, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, nil, nil, nil, dependencies{}); err != nil {
		t.Fatal(err)
	}
}

func TestRunValidatesBeforeStorage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     config.Config
		message string
	}{
		{"model", config.Config{}, "missing required LLM_GATEWAY_MODEL environment variable"},
		{"url", config.Config{LLMGatewayModel: "test-model"}, "missing required LLM_GATEWAY_URL configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := config.NewLogger(config.LevelDebug)
			log.SetOutput(io.Discard)
			path := filepath.Join(t.TempDir(), "unused.db")
			for _, invoke := range []func() error{
				func() error { return Run(context.Background(), &tc.cfg, log, io.Discard) },
				func() error {
					return run(context.Background(), &tc.cfg, log, io.Discard, dependencies{databasePath: path})
				},
			} {
				var startupErr *Error
				if err := invoke(); !errors.As(err, &startupErr) || startupErr.Event != "app.config.invalid" || startupErr.Message != tc.message || startupErr.Cause != nil {
					t.Fatalf("validation error = %#v", err)
				}
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("validation touched database: %v", err)
			}
		})
	}
}

func TestRunLifecycle(t *testing.T) {
	for _, mode := range []string{"registry failure", "registry cancellation", "gateway failure", "gateway cancellation", "success", "gateway start failure", "Home Assistant start failure", "Discord start failure", "iMessage start failure"} {
		t.Run(mode, func(t *testing.T) {
			startFailure := strings.HasSuffix(mode, "start failure")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			logs := &startupLogWriter{gatewayStopped: make(chan struct{}, 1)}
			log := config.NewLogger(config.LevelDebug)
			log.SetOutput(logs)
			cfg := &config.Config{
				LLMGatewayModel: "test-model", LLMGatewayURL: "http://unused.invalid",
				MCPConfigEncryptionKey: "12345678901234567890123456789012", WorkerPoolSize: 1,
			}
			cause := errors.New("factory failed")
			var userStore *memory.Store
			var globalStore *global.Store
			var accountService *accounts.Service
			var runtimeDeps gatewayruntime.Dependencies
			started := make(chan *broker.Broker, 1)
			gatewayDone := make(chan struct{})
			gw := &startupTestGateway{start: func(b *broker.Broker) error {
				defer close(gatewayDone)
				started <- b
				if startFailure {
					return cause
				}
				<-ctx.Done()
				return nil
			}}
			gw.name = strings.TrimSuffix(mode, " start failure")
			if mode == "gateway start failure" {
				gw.name = "test"
			}
			deps := dependencies{
				databasePath: filepath.Join(t.TempDir(), "oswald.db"),
				newRegistry: func(_ *config.Config, m *memory.Store, g *global.Store, l *config.Logger) (*registry.Registry, error) {
					userStore, globalStore = m, g
					if mode == "registry failure" {
						return nil, cause
					}
					if mode == "registry cancellation" {
						cancel()
					}
					return registry.New(l), nil
				},
				newGateways: func(_ *config.Config, a *accounts.Service, d gatewayruntime.Dependencies, _ *config.Logger) ([]gateway.Service, error) {
					accountService, runtimeDeps = a, d
					if mode == "gateway failure" {
						return nil, cause
					}
					if mode == "gateway cancellation" {
						cancel()
					}
					return []gateway.Service{gw}, nil
				},
			}
			done := make(chan error, 1)
			go func() { done <- run(ctx, cfg, log, io.Discard, deps) }()
			completed := false
			t.Cleanup(func() {
				cancel()
				if !completed {
					select {
					case <-done:
					case <-time.After(10 * time.Second):
						t.Error("Run did not finish cleanup")
					}
				}
				if logs.hasEvent("app.start") {
					select {
					case <-gatewayDone:
					case <-time.After(10 * time.Second):
						t.Error("fake gateway did not finish cleanup")
					}
				}
			})
			active := mode == "success" || startFailure
			var startedBroker *broker.Broker
			if active {
				select {
				case startedBroker = <-started:
				case err := <-done:
					completed = true
					t.Fatalf("Run exited before gateway start: %v", err)
				case <-time.After(10 * time.Second):
					t.Fatal("gateway did not start")
				}
				if startFailure {
					select {
					case <-logs.gatewayStopped:
					case <-time.After(10 * time.Second):
						t.Fatal("gateway failure was not logged")
					}
				}
				select {
				case err := <-done:
					completed = true
					t.Fatalf("Run exited before cancellation: %v", err)
				default:
				}
				cancel()
				select {
				case <-gatewayDone:
				case <-time.After(10 * time.Second):
					t.Fatal("fake gateway did not return")
				}
			}
			var err error
			select {
			case err = <-done:
				completed = true
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return")
			}
			wantEvent := ""
			wantMessage := ""
			if mode == "registry failure" {
				wantEvent = "app.tools.init_failed"
				wantMessage = "failed to initialize tools"
			}
			if mode == "gateway failure" {
				wantEvent = "app.gateways.init_failed"
				wantMessage = "failed to initialize gateways"
			}
			if wantEvent != "" {
				var startupErr *Error
				if !errors.Is(err, cause) || !errors.As(err, &startupErr) || startupErr.Event != wantEvent || startupErr.Message != wantMessage {
					t.Fatalf("startup error = %#v, want %s wrapping sentinel", err, wantEvent)
				}
			} else if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if userStore == nil || globalStore == nil {
				t.Fatal("registry did not capture stores")
			}
			_, userErr := userStore.ListMemories("test-user", "", "", 1)
			_, globalErr := globalStore.List(context.Background(), 1)
			assertStartupClosed(t, "user memory", userErr)
			assertStartupClosed(t, "global memory", globalErr)
			if strings.HasPrefix(mode, "registry") {
				if accountService != nil {
					t.Fatal("gateway factory called after registry failure/cancellation")
				}
			} else {
				if accountService == nil || runtimeDeps.Broker == nil || runtimeDeps.Commands == nil || runtimeDeps.Access != accountService || runtimeDeps.Formation == nil || runtimeDeps.Compaction == nil || runtimeDeps.RuntimeInvalidationBus == nil {
					t.Fatal("incomplete gateway dependencies")
				}
				_, accountErr := accountService.HasAdmin()
				assertStartupClosed(t, "accounts", accountErr)
				if err := runtimeDeps.Broker.Submit(&broker.Request{}); !errors.Is(err, broker.ErrShuttingDown) {
					t.Fatalf("broker Submit = %v, want ErrShuttingDown", err)
				}
			}
			if active && startedBroker != runtimeDeps.Broker {
				t.Fatal("gateway received a different broker")
			}
			if !active {
				select {
				case <-started:
					t.Fatal("gateway started after factory failure/cancellation")
				default:
				}
			}
			if got := logs.hasEvent("app.start"); got != active {
				t.Fatalf("app.start logged = %v, want %v", got, active)
			}
			if !logs.hasEvent("app.shutdown") || !logs.hasEvent("app.shutdown.complete") {
				t.Fatal("missing ordered shutdown lifecycle logs")
			}
			logs.mu.Lock()
			cleanupEnd := bytes.LastIndex(logs.buf.Bytes(), []byte(`"event":"app.cleanup.completed"`))
			shutdownEnd := bytes.LastIndex(logs.buf.Bytes(), []byte(`"event":"app.shutdown.complete"`))
			logs.mu.Unlock()
			if cleanupEnd < 0 || shutdownEnd <= cleanupEnd {
				t.Fatal("shutdown completed before cleanup callbacks")
			}
			if got := logs.hasEvent("app.gateway.stopped"); got != startFailure {
				t.Fatalf("gateway failure logged = %v", got)
			}
			if startFailure {
				want := map[string]string{"test": "unknown", "Home Assistant": "homeassistant", "Discord": "discord", "iMessage": "imessage"}[gw.name]
				logs.mu.Lock()
				for _, line := range bytes.Split(logs.buf.Bytes(), []byte("\n")) {
					var record map[string]any
					if json.Unmarshal(line, &record) == nil && record["event"] == "app.gateway.stopped" && record["gateway"] != want {
						t.Errorf("gateway=%v want=%s", record["gateway"], want)
					}
				}
				logs.mu.Unlock()
			}
		})
	}
}

func assertStartupClosed(t *testing.T, name string, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "database is closed") {
		t.Errorf("%s query error = %v, want closed database", name, err)
	}
}

type startupTestGateway struct {
	start func(*broker.Broker) error
	name  string
}

func (g *startupTestGateway) Name() string                 { return g.name }
func (g *startupTestGateway) Start(b *broker.Broker) error { return g.start(b) }

type startupLogWriter struct {
	mu             sync.Mutex
	buf            bytes.Buffer
	gatewayStopped chan struct{}
}

func (w *startupLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	var event struct {
		Event string `json:"event"`
	}
	if json.Unmarshal(p, &event) == nil && event.Event == "app.gateway.stopped" {
		select {
		case w.gatewayStopped <- struct{}{}:
		default:
		}
	}
	return n, err
}

func (w *startupLogWriter) hasEvent(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, line := range bytes.Split(w.buf.Bytes(), []byte("\n")) {
		var event struct {
			Event string `json:"event"`
		}
		if json.Unmarshal(line, &event) == nil && event.Event == name {
			return true
		}
	}
	return false
}

func TestShutdownOrder(t *testing.T) {
	names := []string{"maintenance", "broker", "formation", "compaction", "index", "mcp", "accounts", "mcpStore", "globalMemory", "userMemory"}
	for _, partial := range []bool{false, true} {
		var got, want []string
		var s shutdown
		slots := []*func(){&s.maintenance, &s.broker, &s.formation, &s.compaction, &s.index, &s.mcp, &s.accounts, &s.mcpStore, &s.globalMemory, &s.userMemory}
		for i, slot := range slots {
			if partial && i%2 == 0 {
				continue
			}
			name := names[i]
			want = append(want, name)
			*slot = func() { got = append(got, name) }
		}
		s.run()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("partial=%v shutdown order = %v, want %v", partial, got, want)
		}
	}
	var empty shutdown
	empty.run()
}
