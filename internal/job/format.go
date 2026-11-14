package job

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	configVersion = 1
	planVersion   = 1
)

type Config struct {
	Version           int    `json:"version"`
	ReelPath          string `json:"reel_path"`
	TargetBPS         int64  `json:"target_bps"`
	MinQP             int    `json:"min_qp"`
	MaxQP             int    `json:"max_qp"`
	Concurrency       int    `json:"concurrency"`
	RetryMax          int    `json:"retry_max"`
	CrashAfterSegment int    `json:"crash_after_segment,omitempty"`
}

type ProbeSample struct {
	QP   int     `json:"qp"`
	Bits uint64  `json:"bits"`
	PSNR float64 `json:"psnr_db"`
}

type SegmentPlan struct {
	ID            int           `json:"id"`
	QP            int           `json:"qp"`
	PredictedBits uint64        `json:"predicted_bits"`
	PredictedPSNR float64       `json:"predicted_psnr_db"`
	Probes        []ProbeSample `json:"probes,omitempty"`
}

type Plan struct {
	Version       int           `json:"version"`
	ReelSeed      uint64        `json:"reel_seed"`
	TargetBPS     int64         `json:"target_bps"`
	TargetBits    uint64        `json:"target_bits"`
	DurationSec   float64       `json:"duration_sec"`
	FPS           int           `json:"fps"`
	SegmentFrames int           `json:"segment_frames"`
	TotalFrames   int           `json:"total_frames"`
	MinQP         int           `json:"min_qp"`
	MaxQP         int           `json:"max_qp"`
	Segments      []SegmentPlan `json:"segments"`
}

type Paths struct {
	Root     string
	Config   string
	Plan     string
	Journal  string
	Segments string
	Report   string
}

func NewPaths(root string) Paths {
	return Paths{
		Root:     root,
		Config:   filepath.Join(root, "config.json"),
		Plan:     filepath.Join(root, "plan.json"),
		Journal:  filepath.Join(root, "journal.ndjson"),
		Segments: filepath.Join(root, "segments"),
		Report:   filepath.Join(root, "report.json"),
	}
}

func (p Paths) Prepare() error {
	if err := os.MkdirAll(p.Segments, 0o755); err != nil {
		return err
	}
	return nil
}

func (p Paths) Segment(id int) string {
	return filepath.Join(p.Segments, fmt.Sprintf("seg-%04d.rcxseg", id))
}

func loadConfig(path string) (Config, error) {
	var c Config
	if err := loadJSON(path, &c); err != nil {
		return Config{}, err
	}
	if c.Version != configVersion {
		return Config{}, fmt.Errorf("unsupported job config version %d", c.Version)
	}
	if err := validateConfig(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

func validateConfig(c Config) error {
	if c.TargetBPS <= 0 || c.MinQP < 0 || c.MaxQP > 63 || c.MinQP > c.MaxQP || c.Concurrency < 1 || c.RetryMax < 0 || c.ReelPath == "" {
		return errors.New("invalid job config")
	}
	return nil
}

func loadPlan(path string) (Plan, error) {
	var p Plan
	if err := loadJSON(path, &p); err != nil {
		return Plan{}, err
	}
	if p.Version != planVersion || len(p.Segments) == 0 || p.TargetBits == 0 || p.DurationSec <= 0 {
		return Plan{}, errors.New("invalid plan file")
	}
	return p, nil
}

func loadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".rcx-json-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// ValidateInside enforces the requirement that caller-selected outputs stay
// inside the job directory. Absolute and relative paths are both accepted.
func ValidateInside(root, target string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return err
	}
	if rel == "." || rel == ".." || filepath.IsAbs(rel) || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return fmt.Errorf("output path %q must be inside job directory %q", target, root)
	}
	return nil
}
