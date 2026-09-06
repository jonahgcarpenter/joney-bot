package names

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestBuiltinNamesMatchStableSchemaContract(t *testing.T) {
	contracts := []struct{ name, literal string }{
		{CurrentTime, "time.current"},
		{WebSearch, "web.search"},
		{WebFetch, "web.fetch"},
		{UserMemorySearch, "user_memory_search"},
		{UserMemoryList, "user_memory_list"},
		{UserMemorySave, "user_memory_save"},
		{GlobalMemorySearch, "global_memory_search"},
		{SessionTranscriptSearch, "session_transcript_search"},
		{ComfyUITextToImage, "comfyui.text_to_image"},
		{ComfyUIImageToImage, "comfyui.image_to_image"},
	}
	want := make([]string, 0, len(contracts))
	for _, contract := range contracts {
		if contract.name != contract.literal {
			t.Errorf("builtin name = %q, want stable literal %q", contract.name, contract.literal)
		}
		want = append(want, contract.name)
	}
	slices.Sort(want)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	got := reg.Names()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("loaded schema names = %q, want exactly %q", got, want)
	}
}
