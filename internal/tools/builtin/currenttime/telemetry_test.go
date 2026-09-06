package currenttime

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestInvalidTimezoneErrorIsModelFacingButNotReflectedInLogs(t *testing.T) {
	const canary = "private timezone prose"
	_, err := NewHandler(time.Now)(context.Background(), map[string]interface{}{"timezone": canary})
	if err == nil || !strings.Contains(err.Error(), canary) {
		t.Fatalf("caller error changed: %v", err)
	}
	var output bytes.Buffer
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&output)
	log.Debug("test.currenttime.rejected", "timezone rejected", config.ErrorField(err))
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["event"] != "test.currenttime.rejected" || record["error_code"] == nil {
		t.Fatalf("missing diagnostic: %s", output.String())
	}
	if strings.Contains(output.String(), canary) {
		t.Fatalf("timezone reflected: %s", output.String())
	}
}
