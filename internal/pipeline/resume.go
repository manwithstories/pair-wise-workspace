package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Resume reads the persisted request and continues without simulating another
// interruption.
func Resume(outDir string) (*Result, error) {
	b, err := os.ReadFile(filepath.Join(outDir, "request.json"))
	if err != nil {
		return nil, err
	}
	var req Request
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, err
	}
	req.OutDir = outDir
	req.Interrupt = false
	return Run(req)
}
