package webfetch

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

func mappedClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server, *[]string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	localAddress := server.Listener.Addr().String()
	var dialed []string
	resolve := fakeResolver{
		"public.example":  {netip.MustParseAddr("93.184.216.34")},
		"second.example":  {netip.MustParseAddr("93.184.216.35")},
		"publish.example": {netip.MustParseAddr("93.184.216.36")},
		"x.com":           {netip.MustParseAddr("93.184.216.37")},
	}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		var d net.Dialer
		return d.DialContext(ctx, network, localAddress)
	}
	return newClient(resolve, dial, "http://publish.example/oembed"), server, &dialed
}

func TestClientFetchesAndExtractsHTMLThroughPinnedDial(t *testing.T) {
	client, _, dialed := mappedClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "public.example" || r.Header.Get("User-Agent") != "oswald-ai/web.fetch" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
			t.Fatalf("unsafe request: host=%q headers=%v", r.Host, r.Header)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title> Example Page </title><style>hidden</style></head><body><h1>Hello</h1><script>ignore()</script><p>Readable content.</p></body></html>`))
	})
	response, err := client.Fetch(context.Background(), "http://public.example/page#fragment")
	if err != nil {
		t.Fatal(err)
	}
	if response.URL != "http://public.example/page" || response.Title != "Example Page" || response.Content != "Hello\n\nReadable content." || response.ContentType != "text/html" || response.Source != "direct" {
		t.Fatalf("response = %+v", response)
	}
	if len(*dialed) != 1 || (*dialed)[0] != "93.184.216.34:80" {
		t.Fatalf("dialed = %v", *dialed)
	}
}

func TestClientRevalidatesRedirects(t *testing.T) {
	client, _, _ := mappedClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/private" {
			http.Redirect(w, r, "http://127.0.0.1/secret", http.StatusFound)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/redirect/") {
			var step int
			_, _ = fmt.Sscanf(r.URL.Path, "/redirect/%d", &step)
			http.Redirect(w, r, fmt.Sprintf("http://public.example/redirect/%d", step+1), http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})
	if _, err := client.Fetch(context.Background(), "http://public.example/private"); err == nil {
		t.Fatal("private redirect succeeded")
	}
	if _, err := client.Fetch(context.Background(), "http://public.example/redirect/0"); err == nil {
		t.Fatal("redirect limit was not enforced")
	}
	downgrade, _ := http.NewRequest(http.MethodGet, "http://second.example/page", nil)
	secureOriginal, _ := http.NewRequest(http.MethodGet, "https://public.example/page", nil)
	if err := client.httpClient.CheckRedirect(downgrade, []*http.Request{secureOriginal}); err == nil {
		t.Fatal("HTTPS downgrade succeeded")
	}
}

func TestClientSupportsJSONAndRejectsBinaryAndOversizedBodies(t *testing.T) {
	client, _, _ := mappedClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json":
			w.Header().Set("Content-Type", "application/problem+json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/large":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(strings.Repeat("x", maxBodyBytes+1)))
		default:
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("%PDF"))
		}
	})
	response, err := client.Fetch(context.Background(), "http://public.example/json")
	if err != nil || response.Content != "{\n  \"ok\": true\n}" || response.ContentType != "application/json" {
		t.Fatalf("JSON response=%+v err=%v", response, err)
	}
	if _, err := client.Fetch(context.Background(), "http://public.example/file"); err == nil {
		t.Fatal("binary content succeeded")
	}
	if _, err := client.Fetch(context.Background(), "http://public.example/large"); err == nil {
		t.Fatal("oversized content succeeded")
	}
}

func TestClientUsesXEmbedAndFallsBackToDirectFetch(t *testing.T) {
	adapterFails := false
	client, _, _ := mappedClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "publish.example" {
			if adapterFails {
				http.Error(w, "unavailable", http.StatusBadGateway)
				return
			}
			if r.URL.Query().Get("url") != "https://x.com/alice/status/12345" || r.URL.Query().Get("omit_script") != "true" || r.URL.Query().Get("dnt") != "true" {
				t.Fatalf("unexpected oEmbed query: %v", r.URL.Query())
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"author_name": "Alice", "html": `<blockquote><p>Post content</p></blockquote><script src="ignored"></script>`, "version": "1.0"})
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>Fallback</title></head><body><p>Fallback post</p></body></html>`))
	})
	response, err := client.Fetch(context.Background(), "https://twitter.com/alice/status/12345/photo/1?ref_src=twsrc")
	if err != nil || response.Source != "x_oembed" || response.Title != "X post by Alice" || response.Content != "Post content" || response.URL != "https://x.com/alice/status/12345" {
		t.Fatalf("oEmbed response=%+v err=%v", response, err)
	}
	adapterFails = true
	response, err = client.Fetch(context.Background(), "http://x.com/alice/status/12345")
	if err != nil || response.Source != "direct" || !response.IsDegraded || response.Content != "Fallback post" {
		t.Fatalf("fallback response=%+v err=%v", response, err)
	}
}

func TestEncodeBoundedResponseIsUTF8Safe(t *testing.T) {
	encoded, err := encodeBoundedResponse(Response{URL: "https://example.com/", ContentType: "text/plain", Source: "direct", Content: strings.Repeat("界", maxToolResponseBytes)})
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxToolResponseBytes {
		t.Fatalf("encoded response has %d bytes", len(encoded))
	}
	var response Response
	if err := json.Unmarshal([]byte(encoded), &response); err != nil || !response.IsTruncated || !response.IsDegraded || !strings.HasPrefix(response.Content, "界") {
		t.Fatalf("bounded response=%+v err=%v", response, err)
	}
}

type fakeFetcher struct {
	response Response
	err      error
}

func (f fakeFetcher) Fetch(context.Context, string) (Response, error) { return f.response, f.err }

func TestHandlerRequiresAuthenticationAndClassifiesContent(t *testing.T) {
	handler := NewHandler(fakeFetcher{response: Response{URL: "https://example.com/", ContentType: "text/plain", Source: "direct", Content: "page"}}, config.NewLogger(config.LevelError))
	if _, err := handler(context.Background(), map[string]interface{}{"url": "https://example.com/"}); err == nil {
		t.Fatal("anonymous fetch succeeded")
	}
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "discord", ExternalID: "123", Assurance: identity.AssuranceDiscordGateway}
	ctx := requestctx.WithPrincipal(context.Background(), principal)
	result, err := handler(ctx, map[string]interface{}{"url": "https://example.com/"})
	if err != nil || result.Outcome != governance.OutcomeProductive || !strings.Contains(result.Content, `"content":"page"`) {
		t.Fatalf("result=%+v err=%v", result, err)
	}

	emptyHandler := NewHandler(fakeFetcher{response: Response{URL: "https://example.com/", ContentType: "text/html", Source: "direct"}}, config.NewLogger(config.LevelError))
	result, err = emptyHandler(ctx, map[string]interface{}{"url": "https://example.com/"})
	if err != nil || result.Outcome != governance.OutcomeUnproductive || result.ReasonCode != "no_readable_content" {
		t.Fatalf("empty result=%+v err=%v", result, err)
	}
}

func TestCanonicalXPostURL(t *testing.T) {
	parsed, _ := validateURL("https://mobile.twitter.com/user/status/987/photo/1?lang=en")
	if got, ok := canonicalXPostURL(parsed); !ok || got != "https://x.com/user/status/987" {
		t.Fatalf("canonical X URL = %q, %t", got, ok)
	}
	parsed, _ = validateURL("https://x.com/user/profile")
	if got, ok := canonicalXPostURL(parsed); ok {
		t.Fatalf("non-status X URL accepted: %q", got)
	}
}

func ExampleResponse() {
	response := Response{Source: "direct", ContentType: "text/plain", Content: "example"}
	fmt.Println(response.Source)
	// Output: direct
}
