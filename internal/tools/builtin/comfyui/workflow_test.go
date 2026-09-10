package comfyui

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func workflowPath(name string) string {
	return filepath.Join("..", "..", "..", "..", "data", "workflows", "comfyui", name)
}

func TestBuildMutatesOnlyAllowedWorkflowFields(t *testing.T) {
	for _, test := range []struct {
		mode     Mode
		filename string
	}{
		{mode: TextToImage, filename: "text-to-image-basic.json"},
		{mode: ImageToImage, filename: "image-to-image-basic.json"},
	} {
		t.Run(string(test.mode), func(t *testing.T) {
			workflow, err := LoadWorkflow(workflowPath(test.filename), test.mode)
			if err != nil {
				t.Fatal(err)
			}
			expected := cloneNodes(t, workflow.nodes)
			built, seed, err := workflow.Build("positive", "negative", nil)
			if err != nil {
				t.Fatal(err)
			}
			if test.mode == TextToImage {
				expected["6"].Inputs["text"] = "positive"
				expected["7"].Inputs["text"] = "negative"
				expected["3"].Inputs["seed"] = seed
			} else {
				expected["24"].Inputs["text"] = "positive"
				expected["25"].Inputs["text"] = "negative"
				expected["26"].Inputs["seed"] = seed
				expected["29"].Inputs["image"] = InputImageReference
			}
			if !reflect.DeepEqual(built, expected) {
				t.Fatalf("workflow contained mutations outside the approved fields\nbuilt=%+v\nwant=%+v", built, expected)
			}
			if reflect.DeepEqual(workflow.nodes, built) {
				t.Fatal("immutable template unexpectedly matched the mutated workflow")
			}
		})
	}
}

func cloneNodes(t *testing.T, nodes map[string]node) map[string]node {
	t.Helper()
	data, err := json.Marshal(nodes)
	if err != nil {
		t.Fatal(err)
	}
	var cloned map[string]node
	if err := json.Unmarshal(data, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func TestLoadAndBuildWorkflows(t *testing.T) {
	text, err := LoadWorkflow(workflowPath("text-to-image-basic.json"), TextToImage)
	if err != nil {
		t.Fatal(err)
	}
	built, _, err := text.Build("positive", "negative", nil)
	if err != nil {
		t.Fatal(err)
	}
	if built["6"].Inputs["text"] != "positive" || built["7"].Inputs["text"] != "negative" {
		t.Fatalf("prompts not replaced: %+v", built)
	}
	for key, want := range map[string]interface{}{
		"sampler_name": "dpmpp_2m", "scheduler": "karras",
		"steps": float64(20), "cfg": float64(7), "denoise": float64(1),
	} {
		if got := built["3"].Inputs[key]; got != want {
			t.Errorf("text sampler %s = %v, want %v", key, got, want)
		}
	}
	for key, want := range map[string]interface{}{
		"width": float64(512), "height": float64(512), "batch_size": float64(1),
	} {
		if got := built["5"].Inputs[key]; got != want {
			t.Errorf("text latent %s = %v, want %v", key, got, want)
		}
	}
	seed, ok := built["3"].Inputs["seed"].(uint32)
	if !ok {
		t.Fatalf("seed type = %T, want uint32", built["3"].Inputs["seed"])
	}
	_ = seed
	strength := 0.6
	if _, _, err := text.Build("positive", "negative", &strength); err == nil {
		t.Fatal("text-to-image accepted a denoise override")
	}
	if text.nodes["6"].Inputs["text"] == "positive" {
		t.Fatal("template was mutated")
	}

	image, err := LoadWorkflow(workflowPath("image-to-image-basic.json"), ImageToImage)
	if err != nil {
		t.Fatal(err)
	}
	built, _, err = image.Build("transform", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if built["29"].Inputs["image"] != InputImageReference || built["26"].Inputs["sampler_name"] != "dpmpp_2m" || built["26"].Inputs["scheduler"] != "karras" {
		t.Fatalf("image workflow contract changed: %+v", built)
	}
}

func TestWorkflowValidationRejectsUnsafeBounds(t *testing.T) {
	w, err := LoadWorkflow(workflowPath("text-to-image-basic.json"), TextToImage)
	if err != nil {
		t.Fatal(err)
	}
	w.nodes["5"].Inputs["width"] = float64(4096)
	if err := w.validate(); err == nil {
		t.Fatal("unsafe dimensions accepted")
	}
}
