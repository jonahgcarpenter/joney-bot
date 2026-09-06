package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestNonInteractiveStartupOmitsBanner(t *testing.T) {
	if os.Getenv("OSWALD_STARTUP_TEST_HELPER") == "1" {
		main()
		return
	}
	for _, tc := range []struct{ name, env, message string }{
		{"config loading", "COMFYUI_GENERATION_TIMEOUT=0s", "invalid runtime configuration"},
		{"startup validation", "LLM_GATEWAY_MODEL=", "missing required LLM_GATEWAY_MODEL environment variable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestNonInteractiveStartupOmitsBanner$")
			cmd.Dir = t.TempDir()
			// Both isolated cases fail before any storage or network work.
			cmd.Env = []string{"OSWALD_STARTUP_TEST_HELPER=1", tc.env}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err == nil {
				t.Fatal("expected configuration failure")
			}
			if stdout.Len() != 0 {
				t.Fatalf("expected no banner on piped stdout: %q", stdout.String())
			}
			var event map[string]any
			if err := json.Unmarshal(stderr.Bytes(), &event); err != nil {
				t.Fatalf("stderr is not a single JSON event: %q: %v", stderr.String(), err)
			}
			if event["event"] != "app.config.invalid" || event["msg"] != tc.message || strings.Contains(stderr.String(), "\x1b") {
				t.Fatalf("unexpected startup log: %q", stderr.String())
			}
		})
	}
}
