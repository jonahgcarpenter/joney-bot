package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestModelFailureAfterImagesFinalizesSelectedOutputs(t *testing.T) {
	for _, mode := range []string{"ordinary", "parser_retry", "repeated_parser", "parser_context", "corrective", "tools_disabled"} {
		for _, generated := range []bool{false, true} {
			for _, canceled := range []bool{false, true} {
				for _, streaming := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/images=%t/canceled=%t/stream=%t", mode, generated, canceled, streaming), func(t *testing.T) {
						chat := &fakeChatter{}
						reg := registry.New(config.NewLogger(config.LevelError))
						a, store := newTestAgent(t, chat, nil, reg)
						var logs bytes.Buffer
						a.log = config.NewLogger(config.LevelInfo)
						a.log.SetOutput(&logs)
						policy := testToolPolicy()
						policy.MaxExecutions = 0
						count := 0
						for _, name := range []string{toolnames.ComfyUITextToImage, toolnames.ComfyUIImageToImage, "work"} {
							if err := registerTestTool(t, reg, registry.Spec{Name: name, Description: name}, policy, func(context.Context, map[string]interface{}) (governance.Result, error) {
								count++
								result := governance.Result{Content: `{}`, Outcome: governance.OutcomeProductive}
								if generated {
									image := testInputImage(t, 2+count, 3+count)
									data, _ := base64.StdEncoding.DecodeString(image.Data)
									result.Attachments = []media.OutputAttachment{{Filename: fmt.Sprintf("image-%d.jpg", count), MIMEType: image.MimeType, Data: data}}
								}
								return result, nil
							}); err != nil {
								t.Fatal(err)
							}
						}
						for i, name := range []string{toolnames.ComfyUITextToImage, toolnames.ComfyUIImageToImage} {
							if !generated {
								name = "work"
							}
							chat.outcomes = append(chat.outcomes, fakeChatOutcome{response: toolCallResponse(fmt.Sprint(i), name, map[string]interface{}{"prompt": fmt.Sprint(i)})})
						}
						parserErr := &llm.ChatHTTPError{StatusCode: 500, Body: "XML syntax error on line 7: unexpected EOF"}
						var failure error = errors.New("synthetic provider failure")
						switch mode {
						case "parser_retry", "repeated_parser", "parser_context":
							chat.outcomes = append(chat.outcomes, fakeChatOutcome{err: parserErr})
							if mode == "repeated_parser" {
								failure = parserErr
							}
							if mode == "parser_context" {
								failure = &llm.ChatHTTPError{StatusCode: 400, Body: "maximum context length exceeded"}
							}
						case "corrective":
							chat.outcomes = append(chat.outcomes, fakeChatOutcome{response: &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant"}}})
						case "tools_disabled":
							a.toolPolicy = governance.GlobalPolicy{MaxExecutions: 2, MaxToolIterations: 10}
						}
						if canceled {
							failure = context.Canceled
						}
						chat.outcomes = append(chat.outcomes, fakeChatOutcome{err: failure})
						wantContent, wantKind := generatedImagePartialResponse, "image_partial"
						if mode == "parser_context" {
							wantContent, wantKind = contextCompactionFallback, "context_fallback"
						}
						fallbackChunks := 0
						var callback func(StreamChunk)
						if streaming {
							callback = func(chunk StreamChunk) {
								if len(chunk.Attachments) > 0 {
									t.Fatal("attachments streamed")
								}
								if chunk.Type == ChunkContent && chunk.Text == wantContent {
									fallbackChunks++
								}
							}
						}
						response, err := a.Process(context.Background(), Request{RequestID: "failure", Principal: identity.Principal{CanonicalUserID: "user-1", ExternalID: "user-1", Gateway: "discord", Assurance: identity.AssuranceDiscordGateway}, SessionKey: "session", Prompt: "generate then edit", StreamFunc: callback})
						if canceled {
							if !errors.Is(err, context.Canceled) || response != nil {
								t.Fatalf("response=%v err=%v", response, err)
							}
							var turns int
							if err := store.sql.QueryRow(`SELECT COUNT(*) FROM session_turns`).Scan(&turns); err != nil || turns != 0 {
								t.Fatalf("canceled turns=%d err=%v", turns, err)
							}
							return
						}
						if err != nil || response == nil {
							t.Fatalf("response=%v err=%v", response, err)
						}
						if !generated {
							if mode == "corrective" || mode == "repeated_parser" {
								if response.Response != emptyResponseFallback || response.Error != "" {
									t.Fatal("changed existing empty/parser fallback")
								}
							} else if response.Error == "" || response.SourceTurnID != 0 {
								t.Fatal("changed no-image provider-error semantics")
							}
							return
						}
						if response.Error != "" || response.Response != wantContent || response.Kind != wantKind || response.SourceTurnID == 0 || response.PersistenceStatus != "pending" {
							t.Fatalf("partial response not finalized: %+v", response)
						}
						if len(response.Attachments) != 1 || response.Attachments[0].Filename != "image-2.jpg" {
							t.Fatal("lost selected final image")
						}
						if streaming && fallbackChunks != 1 {
							t.Fatalf("fallback chunks=%d", fallbackChunks)
						}
						if strings.Count(logs.String(), `"event":"agent.response.complete"`) != 1 || !strings.Contains(logs.String(), `"response_kind":"`+wantKind+`"`) {
							t.Fatal("missing partial response measurement")
						}
						images, err := store.SessionImages(context.Background(), "user-1", "session", response.SessionGeneration)
						if err != nil || len(images) != 0 {
							t.Fatal("pending images became editable")
						}
						if err := a.userMemory.MarkSessionTurnDelivered(context.Background(), "user-1", response.SourceTurnID); err != nil {
							t.Fatal(err)
						}
						images, err = store.SessionImages(context.Background(), "user-1", "session", response.SessionGeneration)
						if err != nil || len(images) != 1 || images[0].Version != 2 {
							t.Fatal("delivered selected image unavailable")
						}
						turns, err := store.RecentSessionTurns("user-1", "session", response.SessionGeneration, 10)
						if err != nil || len(turns) != 1 || turns[0].AssistantText != wantContent {
							t.Fatal("partial text not persisted")
						}
					})
				}
			}
		}
	}
}

func TestImageDefaultUsesProductionRecencyNotDeliveryOrder(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, store := newTestAgent(t, chat, nil, reg)
	editArgs := map[string]interface{}{"prompt": "edit A"}
	var logicalA string
	count := 0
	for _, name := range []string{toolnames.ComfyUITextToImage, toolnames.ComfyUIImageToImage} {
		if err := registerTestTool(t, reg, registry.Spec{Name: name, Description: name}, testToolPolicy(), func(ctx context.Context, _ map[string]interface{}) (governance.Result, error) {
			count++
			sources := requestctx.InputImagesFromContext(ctx)
			if count == 2 {
				editArgs["source_image_id"] = sources[0].ID
				logicalA = sources[0].ImageID
			}
			if count == 4 && (sources[0].ImageID != logicalA || sources[0].Version != 2) {
				t.Fatal("default selected B instead of newest A-v2")
			}
			image := testInputImage(t, 2+count, 3+count)
			data, _ := base64.StdEncoding.DecodeString(image.Data)
			return governance.Result{Content: `{}`, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: fmt.Sprintf("image-%d.jpg", count), MIMEType: image.MimeType, Data: data}}}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	chat.responses = []*llm.ChatResponse{toolCallResponse("A", toolnames.ComfyUITextToImage, map[string]interface{}{"prompt": "A"}), toolCallResponse("B", toolnames.ComfyUITextToImage, map[string]interface{}{"prompt": "B"}), toolCallResponse("A2", toolnames.ComfyUIImageToImage, editArgs), {Message: llm.ChatMessage{Role: "assistant", Content: "A and B"}}}
	response, err := processAgent(a, "recency", "discord", "session", "user-1", "User", "generate A B then edit A", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Attachments) != 2 || response.Attachments[0].Filename != "image-3.jpg" || response.Attachments[1].Filename != "image-2.jpg" {
		t.Fatal("delivery order changed")
	}
	images, err := store.SessionImages(context.Background(), "user-1", "session", response.SessionGeneration)
	if err != nil || len(images) != 2 || images[0].ImageID != logicalA || images[0].Version != 2 {
		t.Fatal("storage lost generation recency")
	}
	chat.responses = []*llm.ChatResponse{toolCallResponse("next", toolnames.ComfyUIImageToImage, map[string]interface{}{"prompt": "edit newest"}), {Message: llm.ChatMessage{Role: "assistant", Content: "A-v3"}}}
	if _, err := processAgent(a, "next", "discord", "session", "user-1", "User", "edit", nil, nil); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("calls=%d", count)
	}
}

func TestGeneratedImageVersionsSelectFinalsAndPreserveOtherAttachments(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, store := newTestAgent(t, chat, nil, reg)
	policy := testToolPolicy()
	policy.MaxExecutions = 0
	policy.History.Mode = governance.HistoryMetadata
	ancestorArgs := map[string]interface{}{"prompt": "retry ancestor"}
	variantArgs := map[string]interface{}{"prompt": "blue variant", "create_variant": true}
	var ancestor, originalLogical, variantLogical string
	count := 0
	handler := func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		count++
		sources := requestctx.InputImagesFromContext(ctx)
		if count == 2 {
			ancestor, originalLogical = sources[0].ID, sources[0].ImageID
			ancestorArgs["source_image_id"], variantArgs["source_image_id"] = ancestor, ancestor
		}
		if count == 4 {
			if sources[0].Version != 3 || sources[0].ParentSourceImageID != ancestor || sources[0].ImageID != originalLogical {
				t.Fatal("ancestor retry lost version or parent")
			}
		}
		if count == 5 {
			variantLogical = sources[0].ImageID
			if variantLogical == originalLogical || sources[0].Version != 1 || sources[0].ParentSourceImageID != ancestor {
				t.Fatal("variant did not branch")
			}
		}
		if count == 6 {
			return governance.Result{}, errors.New("synthetic failed edit")
		}
		image := testInputImage(t, 2+count, 3+count)
		data, _ := base64.StdEncoding.DecodeString(image.Data)
		return governance.Result{Content: `{"status":"generated"}`, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: fmt.Sprintf("image-%d.jpg", count), MIMEType: image.MimeType, Data: data}}}, nil
	}
	for _, name := range []string{toolnames.ComfyUITextToImage, toolnames.ComfyUIImageToImage} {
		if err := registerTestTool(t, reg, registry.Spec{Name: name, Description: name}, policy, handler); err != nil {
			t.Fatal(err)
		}
	}
	if err := registerTestTool(t, reg, registry.Spec{Name: "report", Description: "report"}, policy, func(context.Context, map[string]interface{}) (governance.Result, error) {
		return governance.Result{Content: "report", Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: "report.txt", MIMEType: "text/plain", Data: []byte("report")}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	chat.responses = []*llm.ChatResponse{
		toolCallResponse("1", toolnames.ComfyUITextToImage, map[string]interface{}{"prompt": "car"}),
		toolCallResponse("2", toolnames.ComfyUIImageToImage, map[string]interface{}{"prompt": "purple"}),
		toolCallResponse("3", toolnames.ComfyUIImageToImage, ancestorArgs),
		toolCallResponse("report", "report", nil),
		toolCallResponse("4", toolnames.ComfyUIImageToImage, variantArgs),
		toolCallResponse("5", toolnames.ComfyUIImageToImage, map[string]interface{}{"prompt": "improve blue"}),
		toolCallResponse("6", toolnames.ComfyUIImageToImage, map[string]interface{}{"prompt": "failed retry"}),
		{Message: llm.ChatMessage{Role: "assistant", Content: "Two alternatives and report."}},
	}
	response, err := processAgent(a, "versions", "discord", "session", "user-1", "User", "make alternatives", nil, func(chunk StreamChunk) {
		if len(chunk.Attachments) > 0 {
			t.Fatal("attachment streamed before final")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 6 || len(response.Attachments) != 3 {
		t.Fatalf("calls=%d attachments=%d", count, len(response.Attachments))
	}
	for i, want := range []string{"image-3.jpg", "report.txt", "image-5.jpg"} {
		if response.Attachments[i].Filename != want {
			t.Fatalf("slot %d=%s", i, response.Attachments[i].Filename)
		}
	}
	images, err := store.SessionImages(context.Background(), "user-1", "session", response.SessionGeneration)
	if err != nil || len(images) != 2 {
		t.Fatalf("images=%d err=%v", len(images), err)
	}
	if images[0].ImageID != variantLogical || images[0].Version != 2 || images[0].VersionHighwater != 3 || images[1].ImageID != originalLogical || images[1].Version != 3 {
		t.Fatal("wrong final metadata")
	}
	chat.responses = []*llm.ChatResponse{toolCallResponse("next", toolnames.ComfyUIImageToImage, map[string]interface{}{"prompt": "next turn"}), {Message: llm.ChatMessage{Role: "assistant", Content: "Updated."}}}
	response, err = processAgent(a, "next", "discord", "session", "user-1", "User", "edit", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	images, err = store.SessionImages(context.Background(), "user-1", "session", response.SessionGeneration)
	if err != nil || images[0].ImageID != variantLogical || images[0].Version != 4 {
		t.Fatal("cross-turn version highwater reused")
	}
	chat.responses = []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "No new images."}}}
	response, err = processAgent(a, "no-generation", "discord", "session", "user-1", "User", "describe", nil, nil)
	if err != nil || len(response.Attachments) != 0 {
		t.Fatal("loaded source was automatically delivered")
	}
}

func TestGeneratedImageCapacityRejectsBeforeProvider(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, _ := newTestAgent(t, chat, nil, reg)
	policy := testToolPolicy()
	policy.MaxExecutions = 0
	calls := 0
	image := testInputImage(t, 2, 3)
	data, _ := base64.StdEncoding.DecodeString(image.Data)
	if err := registerTestTool(t, reg, registry.Spec{Name: toolnames.ComfyUITextToImage, Description: "generate"}, policy, func(context.Context, map[string]interface{}) (governance.Result, error) {
		calls++
		return governance.Result{Content: `{}`, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: fmt.Sprintf("%d.jpg", calls), MIMEType: image.MimeType, Data: data}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		chat.responses = append(chat.responses, toolCallResponse(fmt.Sprint(i), toolnames.ComfyUITextToImage, map[string]interface{}{"prompt": fmt.Sprint(i)}))
	}
	chat.responses = append(chat.responses, &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", Content: "Four images."}})
	response, err := processAgent(a, "capacity", "discord", "session", "user-1", "User", "generate", nil, nil)
	if err != nil || calls != 4 || len(response.Attachments) != 4 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	result := toolResultByID(chat.requests[len(chat.requests)-1].Messages, "4")
	if result == nil || !strings.Contains(result.Content, "at most four logical images") {
		t.Fatal("missing actionable capacity feedback")
	}
}

func TestGeneratedImagesFeedSuccessiveTextOnlyEdits(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			chat := &fakeChatter{}
			reg := registry.New(config.NewLogger(config.LevelError))
			a, store := newTestAgent(t, chat, nil, reg)
			var logs bytes.Buffer
			level := config.LevelInfo
			if streaming {
				level = config.LevelDebug
			}
			a.log = config.NewLogger(level)
			a.log.SetOutput(&logs)
			var previous string
			count := 0
			handler := func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
				sources := requestctx.InputImagesFromContext(ctx)
				if count > 0 && (len(sources) == 0 || sources[0].Data != previous) {
					t.Fatal("edit did not receive preceding generated bytes")
				}
				image := testInputImage(t, 2+count, 3+count)
				data, err := base64.StdEncoding.DecodeString(image.Data)
				if err != nil {
					t.Fatal(err)
				}
				normalized, err := media.NormalizeInputImageFromBytes(nil, image.MimeType, data, "generated")
				if err != nil {
					t.Fatal(err)
				}
				previous = normalized.Image.Data
				count++
				return governance.Result{Content: `{"status":"generated"}`, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: fmt.Sprintf("image-%d.jpg", count), MIMEType: image.MimeType, Data: data}}}, nil
			}
			policy := testToolPolicy()
			policy.History.Mode = governance.HistoryMetadata
			for _, name := range []string{toolnames.ComfyUITextToImage, toolnames.ComfyUIImageToImage} {
				if err := registerTestTool(t, reg, registry.Spec{Name: name, Description: name}, policy, handler); err != nil {
					t.Fatal(err)
				}
			}
			for i, prompt := range []string{"generate a car", "make it blue", "make it purple"} {
				name := toolnames.ComfyUIImageToImage
				if i == 0 {
					name = toolnames.ComfyUITextToImage
				}
				chat.responses = append(chat.responses, &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "generate", Function: llm.ToolFunction{Name: name, Arguments: map[string]interface{}{"prompt": prompt}}}}}}, &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", Content: "Here it is."}})
				var callback func(StreamChunk)
				if streaming {
					callback = func(StreamChunk) {}
				}
				response, err := processAgent(a, fmt.Sprintf("request-%d", i), "discord", "session", "user-1", "User", prompt, nil, callback)
				if err != nil || response.SourceTurnID == 0 {
					t.Fatalf("response persisted=%v err=%v", response != nil, err)
				}
				final := chat.requests[len(chat.requests)-1]
				if final.Stream != streaming {
					t.Fatal("stream setting changed")
				}
				last := final.Messages[len(final.Messages)-1]
				if len(last.Images) != 1 || last.Images[0].Data != previous || !strings.Contains(last.Content, "Generated image shown:") {
					t.Fatal("final model call did not see normalized output")
				}
				assets, err := store.SessionImages(context.Background(), "user-1", "session", response.SessionGeneration)
				if err != nil || len(assets) != i+1 || assets[0].Data != previous {
					t.Fatalf("retained images=%d err=%v", len(assets), err)
				}
				turns, err := store.RecentSessionTurns("user-1", "session", response.SessionGeneration, 10)
				if err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(turns)
				if strings.Contains(string(encoded), previous) {
					t.Fatal("base64 leaked into durable turn or tool history")
				}
				if strings.Contains(logs.String(), previous) || strings.Contains(logs.String(), prompt) || strings.Contains(logs.String(), assets[0].ID) || strings.Contains(logs.String(), assets[0].ImageID) {
					t.Fatal("private image data leaked into logs")
				}
			}
			for _, event := range []string{"agent.images.loaded", "agent.images.generated", "agent.images.stored"} {
				if strings.Count(logs.String(), `"event":"`+event+`"`) != 3 {
					t.Fatalf("wrong emission count for %s", event)
				}
			}
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var record map[string]interface{}
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record["event"] != "agent.images.generated" {
					continue
				}
				for _, key := range []string{"image_count", "selected_image_count", "catalog_image_count"} {
					if _, ok := record[key].(float64); !ok {
						t.Fatalf("missing numeric %s", key)
					}
				}
			}
		})
	}
}

func TestGeneratedImageContextSurvivesCompaction(t *testing.T) {
	compactor := &fakeForegroundCompactor{artifact: memory.SummaryArtifact{Narrative: "Generated an image."}}
	state := newForegroundCompactionState(compactor, 100, "policy", "", "edit it", nil, nil, []memory.SessionTurn{{ID: 1, UserText: "old", AssistantText: "answer"}}, nil)
	image := requestctx.InputImage{ID: "opaque-id", MIMEType: "image/png", Data: "image-payload", Source: "generated"}
	message := sessionImageContext([]requestctx.InputImage{image}, []requestctx.InputImage{image})
	state.imageContext = &message
	rebuilt, stats, err := state.prepare(context.Background(), []llm.ChatMessage{message}, nil, true)
	if err != nil || !stats.Compacted {
		t.Fatalf("compacted=%v err=%v", stats.Compacted, err)
	}
	last := rebuilt[len(rebuilt)-1]
	if len(last.Images) != 1 || last.Images[0].Data != image.Data || !strings.Contains(last.Content, image.ID) {
		t.Fatal("compaction lost active image")
	}
	encoded, _ := json.Marshal(compactor.calls)
	if strings.Contains(string(encoded), image.Data) {
		t.Fatal("image bytes entered compaction debt")
	}
}

func TestGeneratedImagesChainWithinBatchAndReachToolsDisabledFinal(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, store := newTestAgent(t, chat, nil, reg)
	a.toolPolicy = governance.GlobalPolicy{MaxExecutions: 6, MaxToolIterations: 10}
	current := testInputImage(t, 2, 3)
	previous := current.Data
	count := 0
	policy := testToolPolicy()
	policy.MaxExecutions = 0
	policy.History.Mode = governance.HistoryMetadata
	err := registerTestTool(t, reg, registry.Spec{Name: toolnames.ComfyUIImageToImage, Description: "edit"}, policy, func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		sources := requestctx.InputImagesFromContext(ctx)
		if len(sources) == 0 || sources[0].Data != previous {
			t.Fatal("default source did not advance within batch")
		}
		if count == 0 && sources[0].ID != "current-1" {
			t.Fatal("current image has no selector")
		}
		image := testInputImage(t, 3+count, 4+count)
		data, _ := base64.StdEncoding.DecodeString(image.Data)
		normalized, err := media.NormalizeInputImageFromBytes(nil, image.MimeType, data, "generated")
		if err != nil {
			t.Fatal(err)
		}
		previous = normalized.Image.Data
		count++
		return governance.Result{Content: `{"status":"generated"}`, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: fmt.Sprintf("image-%d.jpg", count), MIMEType: image.MimeType, Data: data}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls []llm.ToolCall
	for i := 0; i < 6; i++ {
		calls = append(calls, llm.ToolCall{ID: fmt.Sprint(i), Function: llm.ToolFunction{Name: toolnames.ComfyUIImageToImage, Arguments: map[string]interface{}{"prompt": fmt.Sprintf("edit %d", i)}}})
	}
	chat.responses = []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", ToolCalls: calls}}, {Message: llm.ChatMessage{Role: "assistant", Content: "Final image."}}}
	response, err := processAgent(a, "batch", "discord", "session", "user-1", "User", "edit repeatedly", []llm.InputImage{current}, nil)
	if err != nil || response == nil || count != 6 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	final := chat.requests[len(chat.requests)-1]
	if len(final.Tools) != 0 {
		t.Fatal("final tools not disabled")
	}
	last := final.Messages[len(final.Messages)-1]
	if len(last.Images) != 4 || last.Images[3].Data != previous {
		t.Fatal("latest four generated outputs not retained for active vision")
	}
	results := 0
	for _, message := range final.Messages {
		if message.Role == "tool" {
			results++
		}
	}
	if results != 6 {
		t.Fatalf("correlated tool results=%d", results)
	}
	assets, err := store.SessionImages(context.Background(), "user-1", "session", response.SessionGeneration)
	if err != nil || len(assets) != 1 || assets[0].Data != previous || assets[0].Version != 6 || len(response.Attachments) != 1 {
		t.Fatalf("assets=%d err=%v", len(assets), err)
	}
}

func TestGeneratedImageCannotSucceedAfterSessionResetBeforePersistence(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{toolCallResponse("generate", toolnames.ComfyUITextToImage, nil), {Message: llm.ChatMessage{Role: "assistant", Content: "Here it is."}}}}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, store := newTestAgent(t, chat, nil, reg)
	image := testInputImage(t, 2, 3)
	data, _ := base64.StdEncoding.DecodeString(image.Data)
	if err := registerTestTool(t, reg, registry.Spec{Name: toolnames.ComfyUITextToImage, Description: "generate"}, testToolPolicy(), func(ctx context.Context, _ map[string]interface{}) (governance.Result, error) {
		if err := store.ResetSessionContext(ctx, "user-1", "session"); err != nil {
			t.Fatal(err)
		}
		return governance.Result{Content: `{"status":"generated"}`, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: "output.jpg", MIMEType: image.MimeType, Data: data}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	response, err := processAgent(a, "reset", "discord", "session", "user-1", "User", "generate", nil, nil)
	if err == nil || response != nil {
		t.Fatal("unpersisted generated image reported as delivered")
	}
	var count int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM session_images`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unfenced images=%d err=%v", count, err)
	}
}
