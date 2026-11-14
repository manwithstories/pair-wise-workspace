package job

import (
	"path/filepath"
	"testing"
)

func TestValidateInside(t *testing.T) {
	root := t.TempDir()
	if err := ValidateInside(root, filepath.Join(root, "report.json")); err != nil {
		t.Fatalf("inside path rejected: %v", err)
	}
	if err := ValidateInside(root, filepath.Join(root, "..", "outside.json")); err == nil {
		t.Fatal("outside path accepted")
	}
}
