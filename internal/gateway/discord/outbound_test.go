package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

type outboundTransport func(*http.Request) (*http.Response, error)

func (f outboundTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOutboundFIFOAndAmbiguousCreate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls []string
		var mu sync.Mutex
		var nonce string
		dg := &Gateway{Log: config.NewLogger(config.LevelInfo)}
		var logs bytes.Buffer
		dg.Log.SetOutput(&logs)
		dg.HTTPClient = &http.Client{Transport: outboundTransport(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			var p map[string]any
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				t.Fatal(err)
			}
			calls = append(calls, p["content"].(string))
			if p["enforce_nonce"] != true {
				t.Fatal("nonce enforcement missing")
			}
			if len(calls) == 1 {
				nonce = p["nonce"].(string)
				return nil, io.ErrUnexpectedEOF
			}
			if len(calls) == 2 && p["nonce"] != nonce {
				t.Fatal("ambiguous create nonce changed")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"sent"}`)), Header: make(http.Header)}, nil
		})}
		first, second := make(chan error, 1), make(chan error, 1)
		go func() {
			first <- newRuntimeResponder(dg, "one", "channel", "", "session", "user").SendAgentResponse(&agent.Response{Response: "first"})
		}()
		synctest.Wait()
		go func() {
			second <- newRuntimeResponder(dg, "two", "channel", "", "session", "user").SendAgentResponse(&agent.Response{Response: "second"})
		}()
		synctest.Wait()
		mu.Lock()
		if len(calls) != 1 {
			t.Fatalf("backlog bypassed: %v", calls)
		}
		mu.Unlock()
		time.Sleep(time.Second)
		synctest.Wait()
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		if err := <-second; err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(calls) != "[first first second]" {
			t.Fatal(calls)
		}
		dg.StopOutbound()
		if strings.Contains(logs.String(), "first") || strings.Contains(logs.String(), "second") {
			t.Fatal("response leaked into logs")
		}
		counts := map[string]int{}
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatal(err)
			}
			counts[record["event"].(string)]++
		}
		if counts["gateway.outbound.complete"] != 2 || counts["gateway.outbound.retry"] != 1 {
			t.Fatal(counts)
		}
	})
}

func TestOutboundAttachmentLimitAndProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dg := &Gateway{Log: config.NewLogger(config.LevelError), outboundBytes: 80 << 20}
		attachment := media.OutputAttachment{Filename: "one.txt", MIMEType: "text/plain", Data: []byte("private attachment")}
		response := &agent.Response{Response: strings.Repeat("chunk ", 500), Attachments: []media.OutputAttachment{attachment}}
		if err := newRuntimeResponder(dg, "overflow", "channel", "", "session", "user").SendAgentResponse(response); err == nil {
			t.Fatal("accepted attachment beyond aggregate cap")
		}
		dg.outboundBytes = 0
		attachments, creates := 0, 0
		var retryNonce string
		dg.HTTPClient = &http.Client{Transport: outboundTransport(func(r *http.Request) (*http.Response, error) {
			if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
				attachments++
				if err := r.ParseMultipartForm(1024); err != nil {
					t.Fatal(err)
				}
				var payload map[string]any
				if err := json.Unmarshal([]byte(r.FormValue("payload_json")), &payload); err != nil {
					t.Fatal(err)
				}
				if payload["enforce_nonce"] != true || payload["nonce"] == "" {
					t.Fatal("attachment nonce missing")
				}
			} else {
				creates++
				var p map[string]any
				if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
					t.Fatal(err)
				}
				if creates == 2 {
					retryNonce = p["nonce"].(string)
					return nil, io.ErrUnexpectedEOF
				}
				if creates == 3 && p["nonce"] != retryNonce {
					t.Fatal("chunk nonce changed")
				}
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"id":"sent-%d"}`, creates))), Header: make(http.Header)}, nil
		})}
		if err := newRuntimeResponder(dg, "request", "channel", "", "session", "user").SendAgentResponse(response); err != nil {
			t.Fatal(err)
		}
		if attachments != 1 || creates != 3 {
			t.Fatalf("attachments=%d creates=%d", attachments, creates)
		}
		dg.StopOutbound()
	})
}

func TestOutboundCapacityExpiryAndShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dg := &Gateway{Log: config.NewLogger(config.LevelError)}
		calls := 0
		dg.HTTPClient = &http.Client{Transport: outboundTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		})}
		results := make(chan error, 20)
		for i := range 20 {
			go func() {
				results <- newRuntimeResponder(dg, fmt.Sprint(i), "channel", "", "session", "user").SendAgentResponse(&agent.Response{Response: "answer"})
			}()
			synctest.Wait()
		}
		if err := newRuntimeResponder(dg, "overflow", "channel", "", "session", "user").SendAgentResponse(&agent.Response{Response: "answer"}); err == nil {
			t.Fatal("accepted 21st entry")
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		for range 20 {
			if err := <-results; err != context.DeadlineExceeded {
				t.Fatalf("expiry: %v", err)
			}
		}
		if calls > 20 {
			t.Fatalf("unbounded attempts: %d", calls)
		}
		go func() {
			results <- newRuntimeResponder(dg, "shutdown", "channel", "", "session", "user").SendAgentResponse(&agent.Response{Response: "answer"})
		}()
		synctest.Wait()
		dg.StopOutbound()
		if err := <-results; err == nil {
			t.Fatal("shutdown acknowledged success")
		}
		if len(dg.outbound) != 0 || dg.outboundBytes != 0 {
			t.Fatal("retained queue")
		}
	})
}

func TestOutboundTransientEditPreservesProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dg := &Gateway{Log: config.NewLogger(config.LevelError)}
		calls := 0
		dg.HTTPClient = &http.Client{Transport: outboundTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodPatch || !strings.HasSuffix(r.URL.Path, "/existing") {
				t.Fatalf("replaced lifecycle: %s %s", r.Method, r.URL.Path)
			}
			calls++
			status := 200
			if calls == 1 {
				status = 502
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"id":"existing"}`)), Header: make(http.Header)}, nil
		})}
		r := newRuntimeResponder(dg, "request", "channel", "", "session", "user")
		state := &discordStreamState{stream: r.stream, messages: []discordLifecycleMessage{{id: "existing", lastDisplay: "preview"}}}
		if err := dg.deliverFinal(state, &agent.Response{Response: "final"}); err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Fatalf("calls=%d", calls)
		}
		dg.StopOutbound()
	})
}

func TestOutboundBoundsAndCancelsInFlightAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dg := &Gateway{Log: config.NewLogger(config.LevelError)}
		attempts := make(chan time.Time, 2)
		dg.HTTPClient = &http.Client{Transport: outboundTransport(func(r *http.Request) (*http.Response, error) {
			deadline, ok := r.Context().Deadline()
			if !ok || time.Until(deadline) > 15*time.Second {
				t.Error("unbounded attempt")
			}
			attempts <- time.Now()
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}
		result := make(chan error, 1)
		go func() {
			result <- newRuntimeResponder(dg, "request", "channel", "", "session", "user").SendAgentResponse(&agent.Response{Response: "answer"})
		}()
		first := <-attempts
		time.Sleep(16 * time.Second)
		second := <-attempts
		if second.Sub(first) != 16*time.Second {
			t.Fatalf("retry timing: %s", second.Sub(first))
		}
		dg.StopOutbound()
		if err := <-result; err != context.Canceled {
			t.Fatalf("shutdown error: %v", err)
		}
	})
}

func TestOutboundHonorsRetryAfter(t *testing.T) {
	for _, multipart := range []bool{false, true} {
		for _, header := range []bool{false, true} {
			for _, seconds := range []string{"45.25", "1e300"} {
				t.Run(fmt.Sprintf("multipart=%t/header=%t/seconds=%s", multipart, header, seconds), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						dg := &Gateway{Log: config.NewLogger(config.LevelError)}
						defer dg.StopOutbound()
						var attempts []time.Time
						dg.HTTPClient = &http.Client{Transport: outboundTransport(func(r *http.Request) (*http.Response, error) {
							status, body := 200, `{"id":"sent"}`
							headers := make(http.Header)
							if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") == multipart {
								attempts = append(attempts, time.Now())
								if len(attempts) == 1 {
									status, body = 429, `{"retry_after":`+seconds+`}`
									if header {
										headers.Set("Retry-After", seconds)
										body = `{"retry_after":1}`
									}
								}
							}
							return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: headers}, nil
						})}
						response := &agent.Response{Response: "final text"}
						if multipart {
							response.Attachments = []media.OutputAttachment{{Filename: "one.txt", MIMEType: "text/plain", Data: []byte("attachment")}}
						}
						start := time.Now()
						err := newRuntimeResponder(dg, "request", "channel", "", "session", "user").SendAgentResponse(response)
						if seconds == "1e300" {
							if !errors.Is(err, context.DeadlineExceeded) || len(attempts) != 1 || time.Since(start) != 5*time.Minute {
								t.Fatalf("expiry err=%v attempts=%v elapsed=%s", err, attempts, time.Since(start))
							}
						} else if err != nil || len(attempts) != 2 || attempts[1].Sub(attempts[0]) != 45250*time.Millisecond {
							t.Fatalf("retry err=%v attempts=%v", err, attempts)
						}
					})
				})
			}
		}
	}
}

func TestOutboundPermanentAttachmentFailureStillDeliversText(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run(fmt.Sprintf("streamed=%t", streamed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dg := &Gateway{Log: config.NewLogger(config.LevelError)}
				defer dg.StopOutbound()
				attachments, creates := 0, 0
				var delivered []string
				var nonce string
				dg.HTTPClient = &http.Client{Transport: outboundTransport(func(r *http.Request) (*http.Response, error) {
					if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
						attachments++
						return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
					}
					var p map[string]any
					if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
						t.Error(err)
					}
					creates++
					if creates == 2 {
						nonce, _ = p["nonce"].(string)
						return nil, io.ErrUnexpectedEOF
					}
					if creates == 3 && p["nonce"] != nonce {
						t.Error("retry changed nonce")
					}
					delivered = append(delivered, p["content"].(string))
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"id":"sent-%d"}`, creates))), Header: make(http.Header)}, nil
				})}
				r := newRuntimeResponder(dg, "request", "channel", "", "session", "user")
				attachment := media.OutputAttachment{Filename: "one.txt", MIMEType: "text/plain", Data: []byte("attachment")}
				if streamed {
					r.Stream(agent.StreamChunk{Type: agent.ChunkToolResult, Attachments: []media.OutputAttachment{attachment}})
				}
				text := strings.Repeat("chunk ", 500)
				err := r.SendAgentResponse(&agent.Response{Response: text, Attachments: []media.OutputAttachment{attachment}})
				if !errors.Is(err, discordHTTPError(403)) {
					t.Fatalf("attachment failure lost: %v", err)
				}
				if attachments != 1 || creates != 3 || fmt.Sprint(delivered) != fmt.Sprint(splitMessage(text, 2000)) {
					t.Fatalf("attachments=%d creates=%d delivered=%v", attachments, creates, delivered)
				}
			})
		})
	}
}

func TestOutboundAgentErrorIgnoresStreamedAttachmentInventory(t *testing.T) {
	for _, attachmentStatus := range []int{200, 403} {
		t.Run(fmt.Sprint(attachmentStatus), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dg := &Gateway{Log: config.NewLogger(config.LevelError)}
				defer dg.StopOutbound()
				attachments, texts := 0, 0
				dg.HTTPClient = &http.Client{Transport: outboundTransport(func(r *http.Request) (*http.Response, error) {
					status := 200
					if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
						attachments++
						status = attachmentStatus
					} else {
						texts++
						var p map[string]any
						if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
							t.Error(err)
						}
						if p["content"] != "safe error text" {
							t.Errorf("text=%v", p["content"])
						}
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"id":"sent"}`)), Header: make(http.Header)}, nil
				})}
				r := newRuntimeResponder(dg, "request", "channel", "", "session", "user")
				r.Stream(agent.StreamChunk{Type: agent.ChunkToolResult, Attachments: []media.OutputAttachment{{Filename: "one.txt", MIMEType: "text/plain", Data: []byte("attachment")}}})
				if err := r.SendAgentError("safe error text"); err != nil {
					t.Fatal(err)
				}
				if attachments != 1 || texts != 1 {
					t.Fatalf("attachments=%d texts=%d", attachments, texts)
				}
			})
		})
	}
}
