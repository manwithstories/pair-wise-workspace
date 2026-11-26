package cache

import (
	"encoding/json"
	"os"
	"testing"
)

func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
