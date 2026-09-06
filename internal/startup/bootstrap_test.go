package startup

import (
	"bytes"
	"errors"
	"testing"
)

func TestPrintBootstrapInstructions(t *testing.T) {
	var out bytes.Buffer
	printBootstrapInstructions(&out, "test-code")
	want := "\nOswald first-administrator bootstrap\n\nBootstrap code: test-code\n\nRun /bootstrap test-code from an authenticated Discord, iMessage, or Home Assistant account. The code is valid once for this process. Restart Oswald to replace a lost code while no administrator exists.\n\n"
	if out.String() != want {
		t.Fatalf("instructions = %q, want %q", out.String(), want)
	}
	w := &failingBootstrapWriter{}
	printBootstrapInstructions(w, "")
	if w.calls != 0 {
		t.Fatal("empty code wrote instructions")
	}
	printBootstrapInstructions(w, "test-code")
	if w.calls != 1 {
		t.Fatalf("failing writer calls = %d, want 1", w.calls)
	}
}

type failingBootstrapWriter struct{ calls int }

func (w *failingBootstrapWriter) Write([]byte) (int, error) {
	w.calls++
	return 0, errors.New("writer unavailable")
}
