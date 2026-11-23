package pipeline

import (
	"os"
	"path/filepath"

	"mediasync/internal/manifest"
	trackmap "mediasync/internal/map"
	"mediasync/internal/model"
	"mediasync/internal/mp4"
	"mediasync/internal/mxf"
	rtprecover "mediasync/internal/rtp"
)

// readSource parses the selected input. MP4 is preferred; MXF is used for the
// clean path when MP4 is absent, and named corrupt fixtures select one reader.
func readSource(opt Options) ([]*model.Track, []model.TrackStats, string, error) {
	switch opt.Corrupt {
	case "":
		mp4Path := filepath.Join(opt.InputDir, "studio.mp4")
		in, err := mp4.ReadFile(mp4Path)
		if err == nil {
			return in.Tracks, in.Stats, mp4Path, nil
		}
		mxfPath := filepath.Join(opt.InputDir, "field.mxf")
		mx, merr := readMXF(mxfPath)
		if merr == nil {
			return mx.Tracks, mx.Stats, mxfPath, nil
		}
		return nil, nil, "", err
	case "truncated-mp4":
		path := filepath.Join(opt.InputDir, "truncated.mp4")
		in, err := mp4.ReadFile(path)
		if err != nil {
			return nil, nil, path, err
		}
		return in.Tracks, in.Stats, path, nil
	case "invalid-klv":
		path := filepath.Join(opt.InputDir, "invalid-klv.mxf")
		mx, err := readMXF(path)
		if err != nil {
			return nil, nil, path, err
		}
		return mx.Tracks, mx.Stats, path, nil
	default:
		return nil, nil, "", model.NewError(model.ErrParamInvalid, 0, 0, "unknown corrupt fixture %q", opt.Corrupt)
	}
}

func readMXF(path string) (*mxf.Input, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return mxf.NewReader(data).Read()
}

// recoverRTP decodes the generated RTP file, orders packets and converts gaps to
// sample intervals. Duplicates are counted once and never become samples.
func recoverRTP(opt Options, rep *Report) ([]manifest.Interval, error) {
	data, err := os.ReadFile(filepath.Join(opt.InputDir, "rtp-input.bin"))
	if err != nil {
		return nil, err
	}
	raw, err := rtprecover.DecodeStream(data)
	if err != nil {
		return nil, err
	}
	results, err := rtprecover.Recover(raw)
	if err != nil {
		return nil, err
	}
	var gaps []manifest.Interval
	for _, r := range results {
		rep.RTPReceived += r.Received
		rep.RTPDuplicates += r.Duplicate
		rep.RTPOutOfOrder += r.OutOfOrder
		rep.RTPTimestampConflicts += r.TimestampConflicts
		for _, g := range r.Gaps {
			gaps = append(gaps, manifest.Interval{Start: int(g.ExtFrom), End: int(g.ExtTo) + 1})
		}
	}
	return gaps, nil
}

func programThroughput(m *trackmap.Result) (float64, float64) {
	var total float64
	var secs float64
	if m == nil {
		return 0, 1
	}
	for _, sel := range m.Selections {
		tr := sel.Track
		var b int64
		for i := range tr.Samples {
			b += int64(tr.Samples[i].Size)
		}
		total += float64(b)
		if tr.Timescale > 0 {
			d := float64(tr.DurationTicks()) / float64(tr.Timescale)
			if d > secs {
				secs = d
			}
		}
	}
	if secs <= 0 {
		secs = 1
	}
	return total, secs
}
