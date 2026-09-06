package registry

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestSkippedDefinitionIsDegradedAndDoesNotReflectSchemaOrName(t *testing.T) {
	const canary = "private_schema_prose"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, canary+".md"), []byte("# "+canary+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&output)
	if _, err := NewFromDirectory(dir, log); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["event"] == "tool.registry.definition_parse_failed" {
			found = true
			if record["level"] != "warn" || record["status"] != "degraded" || record["error_code"] == nil {
				t.Errorf("record=%+v", record)
			}
		}
	}
	if !found || strings.Contains(output.String(), canary) {
		t.Fatalf("logs=%s", output.String())
	}
}
