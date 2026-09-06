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
			if testing.CoverMode() != "" {
				// Let the instrumented child record coverage without inheriting the parent environment.
				cmd.Env = append(cmd.Env, "GOCOVERDIR="+t.TempDir())
			}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err == nil {
				t.Fatal("expected configuration failure")
			}
			if stdout.Len() != 0 {
				t.Fatalf("expected no banner on piped stdout: %q", stdout.String())
			}
			var event map[string]any
			failureCount := 0
			cleanupComplete := false
			for _, line := range bytes.Split(bytes.TrimSpace(stderr.Bytes()), []byte("\n")) {
				event = nil
				if err := json.Unmarshal(line, &event); err != nil {
					t.Fatalf("stderr contains a non-JSON event: %q: %v", line, err)
				}
				if event["event"] == "app.shutdown.complete" {
					cleanupComplete = true
				}
				if event["level"] == "error" {
					failureCount++
					if tc.name == "startup validation" && !cleanupComplete {
						t.Fatal("startup failure logged before cleanup completed")
					}
				}
			}
			if failureCount != 1 || event["event"] != "app.config.invalid" || event["msg"] != tc.message || strings.Contains(stderr.String(), "\x1b") {
				t.Fatalf("unexpected startup log: %q", stderr.String())
			}
		})
	}
}
