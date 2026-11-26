package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"mediad/internal/fixture"
	"mediad/internal/metrics"
	"mediad/internal/verify"
)

// SelfTestOptions are the deterministic demo inputs.
type SelfTestOptions struct {
	Seed     int64
	Duration time.Duration
	Loss     float64
	Burst    int
	Out      string
}

// SelfTestReport is written as JSON and rendered as terminal summaries.
type SelfTestReport struct {
	Seed       int64           `json:"seed"`
	Duration   string          `json:"duration"`
	Loss       float64         `json:"loss_rate"`
	Burst      int             `json:"burst"`
	Out        string          `json:"out"`
	OutputPath string          `json:"output_path"`
	Normal     []SampleSummary `json:"normal"`
	Damaged    []SampleSummary `json:"damaged"`
	Recovered  []SampleSummary `json:"recovered"`
	Main       SampleSummary   `json:"main"`
	Metrics    metrics.Metrics `json:"metrics"`
	Verify     *verify.Report  `json:"verify,omitempty"`
}

// RunSelfTest generates fixtures, processes every class, and returns metrics.
func RunSelfTest(opts SelfTestOptions) (*SelfTestReport, error) {
	if opts.Out == "" {
		return nil, internal(errInvalid("selftest output directory is required"))
	}
	if opts.Duration <= 0 {
		opts.Duration = 60 * time.Second
	}
	if opts.Burst <= 0 {
		opts.Burst = 7
	}
	if err := os.MkdirAll(filepath.Join(opts.Out, "recovered"), 0o755); err != nil {
		return nil, internal(err)
	}
	set, err := fixture.GenerateSet(opts.Out, fixture.Config{Seed: opts.Seed, Duration: opts.Duration})
	if err != nil {
		return nil, internal(err)
	}

	report := &SelfTestReport{
		Seed: opts.Seed, Duration: opts.Duration.String(), Loss: opts.Loss, Burst: opts.Burst,
		Out: opts.Out, OutputPath: filepath.Join(opts.Out, "repaired.mp4"),
	}
	procOpts := Options{Seed: opts.Seed, Loss: opts.Loss, Burst: opts.Burst}

	mainRes, err := ProcessFile("combined", "damaged", set.DamagedCombined, report.OutputPath, procOpts)
	if err != nil {
		return report, err
	}
	report.Main = mainRes.Summary
	report.Metrics = mainRes.Metrics
	report.Verify = mainRes.Verify
	report.Damaged = append(report.Damaged, mainRes.Summary)
	report.Recovered = append(report.Recovered, mainRes.Summary)

	normalCases := []struct{ name, path string }{
		{"normal_mp4", set.NormalMP4},
		{"normal_mov", set.NormalMOV},
	}
	for _, c := range normalCases {
		out := filepath.Join(opts.Out, "recovered", c.name+".repaired.mp4")
		res, err := ProcessFile(c.name, "normal", c.path, out, procOpts)
		if err != nil {
			return report, err
		}
		report.Normal = append(report.Normal, res.Summary)
		report.Recovered = append(report.Recovered, res.Summary)
	}

	names := make([]string, 0, len(set.FaultFiles))
	for name := range set.FaultFiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out := filepath.Join(opts.Out, "recovered", name+".repaired.mp4")
		o := procOpts
		o.Salvage = name == "truncated_mdat"
		res, err := ProcessFile(name, "damaged", set.FaultFiles[name], out, o)
		if err != nil {
			return report, err
		}
		report.Damaged = append(report.Damaged, res.Summary)
		report.Recovered = append(report.Recovered, res.Summary)
	}

	if err := report.WriteJSON(filepath.Join(opts.Out, "selftest-report.json")); err != nil {
		return report, internal(err)
	}
	return report, nil
}

// WriteJSON persists the full self-test report.
func (r *SelfTestReport) WriteJSON(path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type simpleError string

func (e simpleError) Error() string { return string(e) }

func errInvalid(msg string) error { return simpleError(msg) }
