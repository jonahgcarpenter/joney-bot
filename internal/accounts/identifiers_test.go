package accounts

import "testing"

func TestNormalizeIdentifier(t *testing.T) {
	tests := []struct {
		name       string
		gateway    string
		identifier string
		want       string
		wantErr    bool
	}{
		{name: "discord mention", gateway: "discord", identifier: "<@!12345>", want: "12345"},
		{name: "imessage phone", gateway: "imessage", identifier: "(555) 123-4567", want: "+5551234567"},
		{name: "imessage international prefix", gateway: "imessage", identifier: "0015551234567", want: "+15551234567"},
		{name: "imessage email", gateway: "imessage", identifier: "User <Me@Example.COM>", want: "me@example.com"},
		{name: "home assistant", gateway: "homeassistant", identifier: " abc_123 ", want: "abc_123"},
		{name: "bad home assistant", gateway: "homeassistant", identifier: "alice local", wantErr: true},
		{name: "bad discord", gateway: "discord", identifier: "alice", wantErr: true},
		{name: "unknown gateway", gateway: "irc", identifier: "alice", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeIdentifier(tt.gateway, tt.identifier)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
