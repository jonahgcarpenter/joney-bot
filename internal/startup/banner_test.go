package startup

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestPrintBanner(t *testing.T) {
	var output bytes.Buffer
	printBanner(&output, true)
	want := "\x1b[95m" + banner + "\x1b[0m\n"
	if output.String() != want {
		t.Fatalf("banner=%q, want %q", output.String(), want)
	}
}

func TestBannerBlockArtLayout(t *testing.T) {
	lines := strings.Split(strings.Trim(banner, "\n"), "\n")
	if len(lines) != 8 || lines[6] != "" {
		t.Fatal("expected six lines of block art, a blank line, and the project URL")
	}
	if strings.TrimSpace(lines[7]) != "https://github.com/jonahgcarpenter/oswald-ai" {
		t.Fatalf("unexpected project URL: %q", lines[7])
	}
	if !strings.Contains(lines[2], "\u2588\u2588\u2588\u2588\u2588\u2557") || !strings.HasSuffix(lines[0], "\u2588\u2588\u2557") {
		t.Fatal("expected the -AI suffix in the banner")
	}
	if !utf8.ValidString(banner) {
		t.Fatal("banner is not valid UTF-8")
	}
	for _, line := range lines {
		if utf8.RuneCountInString(line) >= 80 {
			t.Fatalf("banner exceeds an 80-column terminal: %q", line)
		}
	}
	for _, r := range banner {
		if r != '\n' && !unicode.IsPrint(r) {
			t.Fatalf("unexpected control character %U", r)
		}
	}
}

func TestPrintBannerIgnoresWriteFailure(t *testing.T) {
	w := &failingWriter{}
	printBanner(w, true)
	if w.calls != 1 {
		t.Fatalf("write calls=%d, want one nonfatal attempt", w.calls)
	}
}

type failingWriter struct{ calls int }

func TestPrintBannerSkipsNonTerminal(t *testing.T) {
	w := &failingWriter{}
	printBanner(w, false)
	if w.calls != 0 {
		t.Fatal("non-terminal output attempted a write")
	}
	f, err := os.CreateTemp(t.TempDir(), "redirected")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	PrintBanner(f)
	info, err := f.Stat()
	if err != nil || info.Size() != 0 {
		t.Fatalf("redirected output was not suppressed: info=%v err=%v", info, err)
	}
}

func (w *failingWriter) Write([]byte) (int, error) {
	w.calls++
	return 0, errors.New("output unavailable")
}
