package pipeline

import (
	"fmt"
	"os"
	"path/filepath"

	"medcheck/index"
)

func persistOutputs(workDir, caseName string, idx *index.Index, srt, vtt []byte) (bool, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return false, err
	}
	indexPath := filepath.Join(workDir, fmt.Sprintf("index-%s.json", caseName))
	_, resumed, err := index.ResumeOrBuild(indexPath, func() (*index.Index, error) { return idx, nil })
	if err != nil {
		return resumed, err
	}
	if err := os.WriteFile(filepath.Join(workDir, caseName+".srt"), srt, 0o644); err != nil {
		return resumed, err
	}
	if err := os.WriteFile(filepath.Join(workDir, caseName+".vtt"), vtt, 0o644); err != nil {
		return resumed, err
	}
	return resumed, nil
}
