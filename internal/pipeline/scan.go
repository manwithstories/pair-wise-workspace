package pipeline

import (
	"context"
	"os"

	"github.com/fashionweek/mediad/internal/cache"
	"github.com/fashionweek/mediad/internal/encode"
	"github.com/fashionweek/mediad/internal/hls"
	"github.com/fashionweek/mediad/internal/media"
	"github.com/fashionweek/mediad/internal/mp4"
	"github.com/fashionweek/mediad/internal/provenance"
)

// scanCameras validates paths, streams each container through the MP4 indexer
// and keeps rejected cameras alive in the report rather than aborting the job.
func scanCameras(ctx context.Context, job *JobConfig, opts Options,
	runner *encode.Runner) ([]*CameraResult, int) {
	_ = ctx
	results := make([]*CameraResult, 0, len(job.Cameras))
	code := ExitOK
	for _, in := range job.Cameras {
		res := &CameraResult{Name: in.Name, Path: in.Path}
		if err := ValidateLocalPath(in.Path); err != nil {
			res.Err, code = asMedia(err), ExitRejected
			results = append(results, res)
			continue
		}
		hash, err := cache.HashFile(in.Path)
		if err != nil {
			res.Err, code = asMedia(err), ExitRejected
			results = append(results, res)
			continue
		}
		res.InputSHA = hash
		f, err := os.Open(in.Path)
		if err != nil {
			res.Err, code = media.New(media.PhaseScan, media.KindTool, in.Path, -1,
				"cannot open camera input").Wrap(err), ExitRejected
			results = append(results, res)
			continue
		}
		idx, err := mp4.IndexFile(f)
		f.Close()
		if err != nil {
			me, _ := media.As(err)
			if me == nil {
				me = media.New(media.PhaseScan, media.KindTool, in.Path, -1, err.Error())
			}
			if me.Path == "" {
				me.Path = in.Path
			}
			res.Err, code = me, ExitRejected
			results = append(results, res)
			continue
		}
		res.Index = idx
		res.Accepted = true
		results = append(results, res)
	}
	return results, code
}

func asMedia(err error) *media.Error {
	if me, ok := media.As(err); ok {
		return me
	}
	return media.New(media.PhaseConfig, media.KindTool, "", -1, err.Error())
}

// ladderFor resolves the bitrate parameters for each configured variant.
func ladderFor(job *JobConfig) []hls.Variant { return job.LadderOrDefault() }

// bitrateFor maps a variant onto encoder bitrate strings.
func bitrateFor(v hls.Variant) (video, audio string) {
	switch v.Name {
	case "high":
		return "1200k", "96k"
	case "mid":
		return "700k", "64k"
	default:
		return "400k", "48k"
	}
}

// stubReceipt ensures a rejected camera still appears in the receipt.
func ensureRejectedReceipt(b *provenance.Builder, res *CameraResult) {
	if _, ok := b.Receipt().Cameras[res.Name]; !ok {
		b.RecordCamera(res.Name, res.Path, res.InputSHA, false, nil)
	}
	if res.Err != nil {
		b.Reject(res.Name, res.Err)
	}
}
