package adapter

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"

	"avcore/internal/clock"
)

// FFProbeV1 is the first-generation ffprobe adapter.
type FFProbeV1 struct {
	binary string
}

// NewFFProbeV1 locates ffprobe on PATH (or via an explicit binary path).
func NewFFProbeV1(binary string) (*FFProbeV1, error) {
	if binary == "" {
		binary = "ffprobe"
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return nil, NewError(CodeProbeFailed, "ffprobe not found: "+err.Error(), Source{})
	}
	return &FFProbeV1{binary: path}, nil
}

func (p *FFProbeV1) AdapterVersion() string { return Version }

// probeJSON mirrors the subset of ffprobe's -print_format json schema used.
type probeJSON struct {
	Streams []probeStream `json:"streams"`
	Format  probeFormat   `json:"format"`
}

type probeStream struct {
	Index        int    `json:"index"`
	CodecName    string `json:"codec_name"`
	CodecType    string `json:"codec_type"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	RFrameRate   string `json:"r_frame_rate"`
	AvgFrameRate string `json:"avg_frame_rate"`
	HasBFrames   int    `json:"has_b_frames"`
	SampleRate   string `json:"sample_rate"`
	Channels     int    `json:"channels"`
}

type probeFormat struct {
	FormatName string `json:"format_name"`
	Duration   string `json:"duration"`
	Size       string `json:"size"`
}

func (p *FFProbeV1) runProbe(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, p.binary, args...)
	out, err := cmd.Output()
	if err != nil {
		code := CodeProbeFailed
		if ctx.Err() != nil {
			code = CodeInterrupted
		}
		return nil, NewError(code, "ffprobe invocation failed: "+err.Error(), Source{File: path})
	}
	return out, nil
}

// Inspect parses container metadata into normalized MediaInfo.
func (p *FFProbeV1) Inspect(ctx context.Context, path string) (*MediaInfo, error) {
	if err := ValidateInput(path); err != nil {
		return nil, err
	}
	out, err := p.runProbe(ctx, path, "-v", "error", "-print_format", "json",
		"-show_format", "-show_streams", path)
	if err != nil {
		return nil, err
	}
	var pr probeJSON
	if jerr := json.Unmarshal(out, &pr); jerr != nil {
		return nil, NewError(CodeProbeFailed, "cannot decode ffprobe json: "+jerr.Error(), Source{File: path})
	}
	info := &MediaInfo{
		Adapter:    p.AdapterVersion(),
		Path:       path,
		FormatName: pr.Format.FormatName,
	}
	if info.DurationTicks, err = parseProbeSeconds(pr.Format.Duration); err != nil {
		return nil, NewError(CodeProbeFailed, "bad format duration", Source{File: path})
	}
	info.Size, _ = strconv.ParseInt(pr.Format.Size, 10, 64)
	for i := range pr.Streams {
		st := &pr.Streams[i]
		switch st.CodecType {
		case "video":
			if info.Video != nil {
				continue // avcore processes the first video track only
			}
			rate, rerr := parseRational(st.RFrameRate)
			if rerr != nil {
				return nil, NewError(CodeUnsupported, "unsupported frame rate "+st.RFrameRate, Source{File: path})
			}
			info.Video = &VideoStream{
				Index: st.Index, Codec: st.CodecName, Width: st.Width, Height: st.Height,
				FrameRate: rate, HasBFrames: st.HasBFrames > 0,
			}
		case "audio":
			if info.Audio != nil {
				continue
			}
			sr, _ := strconv.Atoi(st.SampleRate)
			info.Audio = &AudioStream{
				Index: st.Index, Codec: st.CodecName, SampleRate: sr, Channels: st.Channels,
			}
		}
	}
	if info.Video == nil {
		return nil, NewError(CodeUnsupported, "no video stream found", Source{File: path})
	}
	return info, nil
}

// frameJSON is one element of ffprobe -show_frames output.
type frameJSON struct {
	MediaType  string `json:"media_type"`
	KeyFrame   int    `json:"key_frame"`
	PTS        int64  `json:"pts"`
	PTSTime    string `json:"pts_time"`
}

type framesDoc struct {
	Frames []frameJSON `json:"frames"`
}

// Keyframes lists video keyframe positions on the shared 90 kHz time base.
// ffprobe emits pts_time in the stream time base; conversion goes through an
// exact integer fraction rather than float accumulation.
func (p *FFProbeV1) Keyframes(ctx context.Context, path string) ([]Keyframe, error) {
	if err := ValidateInput(path); err != nil {
		return nil, err
	}
	out, err := p.runProbe(ctx, path, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "frame=key_frame,pts_time", "-of", "json", path)
	if err != nil {
		return nil, err
	}
	var doc framesDoc
	if jerr := json.Unmarshal(out, &doc); jerr != nil {
		return nil, NewError(CodeProbeFailed, "cannot decode frame json: "+jerr.Error(), Source{File: path})
	}
	kfs := make([]Keyframe, 0, len(doc.Frames)/30)
	for _, f := range doc.Frames {
		if f.KeyFrame != 1 {
			continue
		}
		ticks, terr := parseProbeSeconds(f.PTSTime)
		if terr != nil {
			return nil, NewError(CodeBadTimestamp, "bad keyframe pts_time "+f.PTSTime, Source{File: path})
		}
		kfs = append(kfs, Keyframe{TimeTicks: ticks})
	}
	if len(kfs) == 0 {
		return nil, NewError(CodeProbeFailed, "no keyframes reported", Source{File: path})
	}
	return kfs, nil
}

// probeParseError is an internal parsing failure before it is promoted to a
// structured adapter Error with a source location.
type probeParseError struct{ msg string }

func (e *probeParseError) Error() string { return e.msg }

// parseRational decodes "num/den" exactly.
func parseRational(s string) (clock.Rate, error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return clock.Rate{}, &probeParseError{"bad rational: " + s}
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil {
		return clock.Rate{}, &probeParseError{"bad numerator: " + s}
	}
	d, err := strconv.Atoi(parts[1])
	if err != nil || d == 0 {
		return clock.Rate{}, &probeParseError{"bad denominator: " + s}
	}
	return clock.Rate{Num: n, Den: d}, nil
}

// parseProbeSeconds converts ffprobe's decimal seconds string to integer
// 90 kHz ticks without float accumulation.
func parseProbeSeconds(s string) (int64, error) {
	if s == "" || s == "N/A" {
		return 0, &probeParseError{"missing duration"}
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	dot := strings.IndexByte(s, '.')
	var whole, frac string
	if dot < 0 {
		whole = s
	} else {
		whole, frac = s[:dot], s[dot+1:]
	}
	sec, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, &probeParseError{"bad seconds: " + s}
	}
	// Normalize the fractional part to nanoseconds, then to exact ticks.
	if len(frac) > 9 {
		frac = frac[:9]
	}
	ns, err := strconv.ParseInt(frac+strings.Repeat("0", 9-len(frac)), 10, 64)
	if err != nil {
		return 0, &probeParseError{"bad fraction: " + s}
	}
	ticks := sec*clock.TicksPerSecond + ns*clock.TicksPerSecond/1_000_000_000
	if neg {
		ticks = -ticks
	}
	return ticks, nil
}
