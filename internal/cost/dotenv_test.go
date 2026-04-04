package cost

import (
	"os"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	f, err := os.CreateTemp("", "env-*")
	if err != nil { t.Fatal(err) }
	defer os.Remove(f.Name())
	f.WriteString("ENTSOE_API_KEY=test-key-123\nOTHER_VAR=hello\n# comment\n\nSPACED = value \n")
	f.Close()

	LoadDotEnv(f.Name())

	if os.Getenv("ENTSOE_API_KEY") != "test-key-123" { t.Errorf("expected test-key-123, got %s", os.Getenv("ENTSOE_API_KEY")) }
	if os.Getenv("OTHER_VAR") != "hello" { t.Errorf("expected hello, got %s", os.Getenv("OTHER_VAR")) }
	if os.Getenv("SPACED") != "value" { t.Errorf("expected 'value', got '%s'", os.Getenv("SPACED")) }

	os.Unsetenv("ENTSOE_API_KEY")
	os.Unsetenv("OTHER_VAR")
	os.Unsetenv("SPACED")
}

func TestLoadDotEnvMissingFile(t *testing.T) {
	LoadDotEnv("/nonexistent/.env") // should not panic
}

func TestLoadDotEnvNoOverride(t *testing.T) {
	os.Setenv("EXISTING_VAR", "original")
	defer os.Unsetenv("EXISTING_VAR")

	f, err := os.CreateTemp("", "env-*")
	if err != nil { t.Fatal(err) }
	defer os.Remove(f.Name())
	f.WriteString("EXISTING_VAR=overwritten\n")
	f.Close()

	LoadDotEnv(f.Name())

	if os.Getenv("EXISTING_VAR") != "original" { t.Errorf("expected original, got %s", os.Getenv("EXISTING_VAR")) }
}
