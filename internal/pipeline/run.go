// Package pipeline orchestrates the full camera-to-HLS delivery: container
// scanning and indexing feed the alignment module, which drives per-segment
// FFmpeg transcoding and fMP4 packaging. A content-addressed cache and atomic
// per-segment checkpoints govern reuse and crash recovery, while a provenance
// receipt ties every output back to its source.
package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/fashionweek/mediad/internal/align"
	"github.com/fashionweek/mediad/internal/cache"
	"github.com/fashionweek/mediad/internal/clock"
	"github.com/fashionweek/mediad/internal/encode"
	"github.com/fashionweek/mediad/internal/media"
	"github.com/fashionweek/mediad/internal/mp4"
	"github.com/fashionweek/mediad/internal/provenance"
)

// Exit codes form the command's public contract.
const (
	ExitOK          = 0
	ExitArgs        = 2
	ExitRejected    = 3
	ExitInterrupted = 75
)

// Options controls a single pipeline invocation.
type Options struct {
	JobPath        string
	WorkDir        string
	Resume         bool
	InterruptAfter int // number of completed segments before synthetic stop; <=0 = off
}

// CameraResult is one camera's outcome for the terminal report.
type CameraResult struct {
	Name     string
	Path     string
	Accepted bool
	InputSHA string
	Timeline *align.Timeline
	Index    *mp4.MovieIndex
	Receipt  *provenance.CameraReceipt
	Err      *media.Error
}

// Report is the serialisable run summary.
type Report struct {
	Status        string                     `json:"status"`
	Accepted      []string                   `json:"accepted_cameras"`
	Rejected      []provenance.Rejection     `json:"rejected"`
	SegmentsDone  int                        `json:"segments_done"`
	SegmentsTotal int                        `json:"segments_total"`
	Outputs       map[string][]string        `json:"outputs"`
	ToolVersion   string                     `json:"ffmpeg_version"`
}

// Run executes the pipeline and returns a structured report and process code.
func Run(ctx context.Context, opts Options) (*Report, int, error) {
	job, err := LoadJob(opts.JobPath)
	if err != nil {
		return nil, ExitArgs, err
	}
	if err := os.MkdirAll(opts.WorkDir, 0o755); err != nil {
		return nil, ExitArgs, media.New(media.PhaseConfig, media.KindTool,
			opts.WorkDir, -1, "cannot create work dir").Wrap(err)
	}
	runner, err := encode.New(ctx)
	if err != nil {
		return nil, ExitArgs, err
	}

	results, _ := scanCameras(ctx, job, opts, runner)
	accepted := make([]*CameraResult, 0, len(results))
	for _, res := range results {
		if res.Accepted {
			accepted = append(accepted, res)
		}
	}
	report := &Report{
		ToolVersion: runner.Version(),
		Outputs:     map[string][]string{},
	}
	for _, res := range results {
		if res.Accepted {
			report.Accepted = append(report.Accepted, res.Name)
		} else if res.Err != nil {
			report.Rejected = append(report.Rejected, provenance.Rejection{
				Camera: res.Name, Kind: res.Err.Kind, Phase: res.Err.Phase,
				Path: res.Err.Path, Offset: res.Err.Offset, Detail: res.Err.Detail,
			})
		}
	}
	if len(accepted) == 0 {
		return report, ExitRejected, nil
	}

	// Align every accepted camera onto one shared wall-clock timeline.
	cams := make([]*align.Camera, 0, len(accepted))
	for _, res := range accepted {
		cams = append(cams, &align.Camera{
			Name: res.Name, Path: res.Path,
			Creation: res.Index.CreationTime, Track: res.Index.Video,
		})
	}
	timeline, err := align.Build(cams)
	if err != nil {
		// Alignment failure rejects every camera; keep prior rejections too.
		if me, ok := media.As(err); ok {
			for _, res := range accepted {
				res.Accepted = false
				res.Err = me
			}
		}
		return report, ExitRejected, err
	}

	segDur, _ := clock.New(int64(job.SegmentSeconds), 1)
	cuts, err := timeline.Plan(segDur)
	if err != nil {
		return report, ExitRejected, err
	}

	cfgHash := configDigest(job, runner.Version())
	builder := provenance.NewBuilder(job.JobID, runner.Version(), cfgHash)
	cas, err := cache.Open(filepath.Join(opts.WorkDir, "cache"))
	if err != nil {
		return nil, ExitArgs, err
	}

	done, total, interrupted, err := encodeAll(ctx, encodeDeps{
		job: job, opts: opts, runner: runner, timeline: timeline,
		cuts: cuts, cas: cas, builder: builder, results: accepted,
		segDur: segDur,
	})
	if err != nil {
		return report, ExitArgs, err
	}
	report.SegmentsDone, report.SegmentsTotal = done, total

	// Persist the receipt regardless of how far we got.
	if err := builder.Write(filepath.Join(opts.WorkDir, "receipt.json")); err != nil {
		return nil, ExitArgs, err
	}
	if interrupted {
		report.Status = "interrupted"
		writeReport(opts.WorkDir, report)
		return report, ExitInterrupted, nil
	}

	// All segments exist: publish manifests. A manifest never references a
	// camera that was rejected, so no playlist can point at a missing segment.
	if err := publishManifests(job, accepted, timeline, cuts, opts.WorkDir); err != nil {
		return nil, ExitArgs, err
	}
	for _, res := range accepted {
		report.Outputs[res.Name] = cameraOutputFiles(opts.WorkDir, res.Name)
	}
	report.Status = "published"
	writeReport(opts.WorkDir, report)
	return report, ExitOK, nil
}

func writeReport(work string, r *Report) {
	data, _ := json.MarshalIndent(r, "", "  ")
	_ = os.WriteFile(filepath.Join(work, "report.json"),
		append(data, '\n'), 0o644)
}

func configDigest(job *JobConfig, toolVersion string) string {
	canonical, _ := json.Marshal(struct {
		Job  *JobConfig `json:"job"`
		Tool string     `json:"tool"`
	}{job, toolVersion})
	return cache.HashBytes(canonical)
}
