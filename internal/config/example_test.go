package config

import "testing"

// The shipped example is documentation people copy; it must always validate.
func TestShippedV2ExampleValidates(t *testing.T) {
	cfg := parseFile(t, "../../viiwork.yaml.example")
	if err := cfg.Validate(envOf(map[string]string{"VIIWORK_MESH_SECRET": testSecret})); err != nil {
		t.Fatalf("viiwork.yaml.example must validate: %v", err)
	}
	if len(cfg.Models) == 0 {
		t.Error("the example must show at least one model")
	}
}
