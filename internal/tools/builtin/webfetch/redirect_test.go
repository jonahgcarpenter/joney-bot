package webfetch

import (
	"context"
	"net/http"
	"net/netip"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type redirectResolver struct {
	mu    sync.Mutex
	hosts []string
	after []netip.Addr
}

func (r *redirectResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hosts = append(r.hosts, host)
	if len(r.hosts) == 1 {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	return r.after, nil
}

func TestRedirectRevalidatesDNSBeforeUnderlyingDial(t *testing.T) {
	for _, host := range []string{"public.example", "second.example"} {
		for _, mixed := range []bool{false, true} {
			name := host + "/private"
			if mixed {
				name = host + "/mixed"
			}
			t.Run(name, func(t *testing.T) {
				var requests atomic.Int32
				client, _, dialed := mappedClient(t, func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					http.Redirect(w, r, "http://"+host+"/redirected", http.StatusFound)
				})
				resolve := &redirectResolver{after: []netip.Addr{netip.MustParseAddr("10.0.0.1")}}
				if mixed {
					resolve.after = append([]netip.Addr{netip.MustParseAddr("93.184.216.35")}, resolve.after...)
				}
				transport := client.httpClient.Transport.(*http.Transport)
				mappedDial := transport.DialContext
				transport.DialContext = secureDialContext(resolve, mappedDial)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				response, err := client.Fetch(ctx, "http://public.example/start")
				if err == nil || response != (Response{}) {
					t.Fatalf("unsafe redirect returned response=%+v err=%v", response, err)
				}
				resolve.mu.Lock()
				defer resolve.mu.Unlock()
				if !reflect.DeepEqual(resolve.hosts, []string{"public.example", host}) {
					t.Fatalf("DNS lookups=%v", resolve.hosts)
				}
				if requests.Load() != 1 || !reflect.DeepEqual(*dialed, []string{"93.184.216.34:80"}) {
					t.Fatalf("requests=%d underlying dials=%v", requests.Load(), *dialed)
				}
			})
		}
	}
}
