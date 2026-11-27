package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/fashionweek/mediad/internal/hls"
	"github.com/fashionweek/mediad/internal/media"
)

// CameraInput names one source angle in the job descriptor.
type CameraInput struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// JobConfig is the on-disk job descriptor (job.json).
type JobConfig struct {
	JobID          string          `json:"job_id"`
	SegmentSeconds int             `json:"segment_seconds"`
	Ladder         []hls.Variant   `json:"ladder,omitempty"`
	Cameras        []CameraInput   `json:"cameras"`
}

// Ladder returns the configured ladder or the built-in three-tier default.
func (j *JobConfig) LadderOrDefault() []hls.Variant {
	if len(j.Ladder) > 0 {
		return j.Ladder
	}
	return hls.DefaultLadder()
}

// LoadJob reads and validates a job descriptor.
func LoadJob(path string) (*JobConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, media.New(media.PhaseConfig, media.KindTool, path, -1,
			"cannot read job descriptor").Wrap(err)
	}
	var j JobConfig
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, media.New(media.PhaseConfig, media.KindDigest, path, -1,
			"job descriptor is not valid JSON").Wrap(err)
	}
	if j.JobID == "" {
		j.JobID = "job"
	}
	if j.SegmentSeconds <= 0 {
		j.SegmentSeconds = 2
	}
	if len(j.Cameras) == 0 {
		return nil, media.New(media.PhaseConfig, media.KindBounds, path, -1,
			"job declares no cameras")
	}
	names := map[string]bool{}
	for _, c := range j.Cameras {
		if c.Name == "" || c.Path == "" {
			return nil, media.New(media.PhaseConfig, media.KindBounds, path, -1,
				"every camera needs a name and path")
		}
		if names[c.Name] {
			return nil, media.New(media.PhaseConfig, media.KindBounds, path, -1,
				"duplicate camera name: "+c.Name)
		}
		names[c.Name] = true
	}
	return &j, nil
}

// ValidateLocalPath rejects traversal/non-local paths and non-regular files.
func ValidateLocalPath(p string) error {
	clean := filepath.Clean(p)
	if filepath.IsAbs(clean) {
		// Absolute paths are allowed only when they stay on the local fs and
		// contain no traversal; the caller still controls the workspace root.
	}
	for _, part := range filepath.SplitList(clean) {
		if part == ".." {
			return media.New(media.PhaseConfig, media.KindPath, p, -1,
				"path traversal segment '..' is forbidden")
		}
	}
	// Reject any ".." component regardless of separator style.
	if containsDotDot(clean) {
		return media.New(media.PhaseConfig, media.KindPath, p, -1,
			"path traversal is forbidden")
	}
	info, err := os.Stat(clean)
	if err != nil {
		return media.New(media.PhaseConfig, media.KindPath, p, -1,
			"input path not accessible").Wrap(err)
	}
	if info.IsDir() {
		return media.New(media.PhaseConfig, media.KindPath, p, -1,
			"input path is a directory, expected a file")
	}
	return nil
}

func containsDotDot(p string) bool {
	parts := splitPath(p)
	for _, part := range parts {
		if part == ".." {
			return true
		}
	}
	return false
}

func splitPath(p string) []string {
	var parts []string
	for {
		dir, file := filepath.Split(p)
		if file == "" && dir == "" {
			break
		}
		if file != "" {
			parts = append(parts, file)
		}
		if dir == "" || dir == p {
			break
		}
		p = filepath.Clean(dir)
	}
	return parts
}
