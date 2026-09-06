package runtime

import (
	"testing"
)

func TestExposureTrimsAndDeduplicatesToolNames(t *testing.T) {
	exposure := NewExposure()
	exposure.ExposeTools([]string{" github.get_issue ", "", "github.get_issue"})
	exposed := exposure.ExposedMCPTools()
	if len(exposed) != 1 || !exposed["github.get_issue"] {
		t.Fatalf("unexpected exposed tools: %+v", exposed)
	}
	delete(exposed, "github.get_issue")
	if !exposure.ExposedMCPTools()["github.get_issue"] {
		t.Fatal("mutating exposed tools snapshot changed request state")
	}
	exposure.HideBuiltins(" comfyui.image_to_image ", "")
	visibility := exposure.Visibility()
	if len(visibility.HiddenBuiltins) != 1 || !visibility.HiddenBuiltins["comfyui.image_to_image"] {
		t.Fatalf("unexpected hidden builtins: %+v", visibility.HiddenBuiltins)
	}
}

func TestNilExposureVisibilityIsEmpty(t *testing.T) {
	var exposure *Exposure
	if exposure.Visibility().HiddenBuiltins != nil || exposure.ExposedMCPTools() != nil {
		t.Fatal("expected empty visibility")
	}
	exposure.ExposeTools([]string{"github.get_issue"})
}
