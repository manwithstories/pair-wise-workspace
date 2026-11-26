package pipeline

import (
	"bytes"
	"errors"
	"image"
	"path/filepath"
	"sort"

	"github.com/disintegration/imaging"

	"mediad/internal/audio"
	"mediad/internal/bitstream"
	"mediad/internal/fault"
	"mediad/internal/manifest"
	"mediad/internal/metrics"
	"mediad/internal/stream"
	"mediad/internal/video"
)

func joinPath(dir, file string) string { return filepath.Join(dir, file) }

func sortFrames(fs []manifest.Frame) []manifest.Frame {
	out := make([]manifest.Frame, len(fs))
	copy(out, fs)
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// verifyFrame reads the frame file within the byte bound, verifies its
// SHA-256 and checks the container structure. It returns ok=false after
// recording a recoverable fault when the frame is corrupt.
func verifyFrame(t *manifest.Track, f manifest.Frame, opts Options, faults *fault.List) ([]byte, bool) {
	data, err := stream.ReadBounded(joinPath(opts.Dir, f.File), opts.MaxFileBytes)
	if err != nil {
		faults.Add(fault.Fault{Track: t.ID, Frame: f.Seq, Kind: fault.KindTruncated,
			Recoverable: true, Detail: "read: " + err.Error()})
		return nil, false
	}
	br := fault.ByteRange{Start: 0, End: int64(len(data))}
	if !stream.VerifySHA256(data, f.SHA256) {
		faults.Add(fault.Fault{Track: t.ID, Frame: f.Seq, Bytes: br,
			Kind: fault.KindHashMismatch, Recoverable: true,
			Detail: "sha256 mismatch"})
		return nil, false
	}
	if err := bitstream.Check(t.Codec, data); err != nil {
		kind := fault.KindStructure
		if errors.Is(err, bitstream.ErrTruncated) {
			kind = fault.KindTruncated
		}
		faults.Add(fault.Fault{Track: t.ID, Frame: f.Seq, Bytes: br,
			Kind: kind, Recoverable: true, Detail: err.Error()})
		return nil, false
	}
	return data, true
}

func decodeFrame(data []byte) (image.Image, error) {
	return imaging.Decode(bytes.NewReader(data))
}

func mseOf(pr video.Result) float64 { return metrics.MSE(pr.Ref, pr.Proxy) }

func psnrOf(mse float64) float64 { return metrics.PSNR(mse) }

func alignMs(got, want []int64, secPerTick float64) float64 {
	return metrics.AlignMaxMs(got, want, secPerTick)
}

// runAudio verifies and normalizes one PCM track, recording the output
// sample-frame count. Unsupported layouts are unrecoverable.
func runAudio(t *manifest.Track, opts Options, res *Result, faults *fault.List) error {
	data, err := stream.ReadBounded(joinPath(opts.Dir, t.File), opts.MaxFileBytes)
	if err != nil {
		return err
	}
	if !stream.VerifySHA256(data, t.SHA256) {
		faults.Add(fault.Fault{Track: t.ID, Frame: -1,
			Bytes: fault.ByteRange{End: int64(len(data))},
			Kind:  fault.KindHashMismatch, Recoverable: true,
			Detail: "sha256 mismatch"})
		return nil
	}
	layout, ok := audio.ParseLayout(t.Layout)
	if !ok {
		faults.Add(fault.Fault{Track: t.ID, Frame: -1,
			Kind: fault.KindUnsupportedLayout, Recoverable: false,
			Detail: "unsupported layout: " + t.Layout})
		return nil
	}
	n, err := audio.Normalize(bytes.NewReader(data), &devNull{}, audio.Spec{
		Layout: layout, Rate: t.SampleRate})
	if err != nil {
		if errors.Is(err, audio.ErrUnsupported) {
			faults.Add(fault.Fault{Track: t.ID, Frame: -1,
				Kind: fault.KindUnsupportedLayout, Recoverable: false,
				Detail: err.Error()})
			return nil
		}
		return err
	}
	res.AudioSamples[t.ID] = n
	return nil
}

type devNull struct{}

func (*devNull) Write(p []byte) (int, error) { return len(p), nil }
