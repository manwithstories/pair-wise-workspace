package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"presspack/internal/clock"
	"presspack/internal/core"
)

// FFprobeV1 probes media with the ffprobe binary (Homebrew ffmpeg).
type FFprobeV1 struct {
	Bin string // ffprobe path; empty resolves via PATH
}

func (FFprobeV1) Version() string { return "ffprobe/" + APIVersion }

// ffprobeJSON mirrors the ffprobe JSON schema we consume.
type ffprobeJSON struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
	Streams []struct {
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		FrameRate string `json:"r_frame_rate"`
		Timebase  string `json:"time_base"`
	} `json:"streams"`
	Packets []struct {
		PTS      int64  `json:"pts"`
		DTS      int64  `json:"dts"`
		Duration int64  `json:"duration"`
		Flags    string `json:"flags"`
		Pos      string `json:"pos"`
	} `json:"packets"`
}

// Probe runs ffprobe and converts its output to the unified model. A
// truncated or damaged file still yields the decodable prefix: ffprobe's
// partial JSON is used and a recoverable FaultCorruptMedia is recorded.
// Only a file with zero decodable samples is a hard error.
func (f FFprobeV1) Probe(ctx context.Context, path string) (*ProbeResult, error) {
	bin := f.Bin
	if bin == "" {
		bin = "ffprobe"
	}
	args := []string{
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "format=format_name,duration,bit_rate:stream=codec_name,width,height,r_frame_rate,time_base:packet=pts,dts,duration,flags,pos",
		"-of", "json", path,
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	runFailed := err != nil
	if runFailed && (!errors.As(err, &exitErr) || len(out) == 0) {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	var pj ffprobeJSON
	if jerr := json.Unmarshal(out, &pj); jerr != nil {
		return nil, fmt.Errorf("ffprobe output unparseable for %s: %w", path, jerr)
	}
	res := &ProbeResult{Path: path}
	res.Format = containerFamily(pj.Format.FormatName)
	if d, perr := strconv.ParseFloat(pj.Format.Duration, 64); perr == nil {
		res.Duration = core.FromMillis(int64(d * 1000))
	}
	res.BitRate, _ = strconv.ParseInt(pj.Format.BitRate, 10, 64)
	if len(pj.Streams) > 0 {
		st := pj.Streams[0]
		res.Width, res.Height = st.Width, st.Height
		if fps, ferr := parseFrameRate(st.FrameRate); ferr == nil {
			res.FPS = fps
		}
	}
	var tb core.Rational
	if len(pj.Streams) > 0 {
		tb = parseTimebase(pj.Streams[0].Timebase)
	}
	if tb.Den == 0 {
		tb = core.Rat(1, core.Timescale)
	}
	for _, p := range pj.Packets {
		s := MediaSample{
			PTS:      core.FromUnits(p.PTS, tb),
			Duration: core.FromUnits(p.Duration, tb),
			Keyframe: strings.Contains(p.Flags, "K"),
			Offset:   -1,
		}
		if pos, perr := strconv.ParseInt(p.Pos, 10, 64); perr == nil {
			s.Offset = pos
		}
		res.Samples = append(res.Samples, s)
	}
	if len(res.Samples) == 0 {
		return nil, fmt.Errorf("ffprobe: no decodable video samples in %s", path)
	}
	if runFailed {
		res.Truncated = true
		res.Faults = append(res.Faults, core.NewFault(core.FaultCorruptMedia,
			lastOffset(res.Samples), true,
			"ffprobe reported errors; decodable prefix kept"))
	}
	return res, nil
}

func lastOffset(samples []MediaSample) int64 {
	for i := len(samples) - 1; i >= 0; i-- {
		if samples[i].Offset >= 0 {
			return samples[i].Offset
		}
	}
	return -1
}

func containerFamily(formatName string) string {
	switch {
	case strings.Contains(formatName, "mp4") || strings.Contains(formatName, "mov"):
		return "mp4"
	case strings.Contains(formatName, "mpegts"):
		return "mpegts"
	default:
		return formatName
	}
}

func parseFrameRate(s string) (clock.FPS, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return clock.FPS{}, fmt.Errorf("bad frame rate %q", s)
	}
	num, err1 := strconv.ParseInt(parts[0], 10, 64)
	den, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || den == 0 {
		return clock.FPS{}, fmt.Errorf("bad frame rate %q", s)
	}
	for _, f := range clock.Supported() {
		if f.Num*den == num*f.Den {
			return f, nil
		}
	}
	return clock.FPS{}, fmt.Errorf("unsupported frame rate %q", s)
}

func parseTimebase(s string) core.Rational {
	var num, den int64
	if _, err := fmt.Sscanf(s, "%d/%d", &num, &den); err != nil || den == 0 {
		return core.Rational{}
	}
	return core.Rat(num, den)
}
