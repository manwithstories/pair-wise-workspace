// Package ffmpeg wraps the locally installed ffmpeg/ffprobe binaries. All
// media handling stays on this machine: there is no network fetch. It only
// ever produces or reads the MP4/H.264/AAC fixtures the manifest declares.
package ffmpeg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ErrNotFound means ffmpeg or ffprobe is not installed on PATH.
var ErrNotFound = errors.New("ffmpeg/ffprobe not found on PATH")

// Binaries resolves the tool paths once; callers check ErrNotFound up front.
func Binaries() (ffmpeg, ffprobe string, err error) {
	ffmpeg, e1 := exec.LookPath("ffmpeg")
	ffprobe, e2 := exec.LookPath("ffprobe")
	if e1 != nil || e2 != nil {
		return "", "", ErrNotFound
	}
	return ffmpeg, ffprobe, nil
}

// run executes a command, capturing stderr for diagnostics.
func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s: %w; %s",
			name, strings.Join(args, " "), err, tail(stderr.String(), 400))
	}
	return stdout.Bytes(), nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// StreamInfo describes one probed elementary stream.
type StreamInfo struct {
	Index        int
	CodecType    string // video / audio
	CodecName    string // h264 / aac
	Width        int
	Height       int
	TimebaseNum  int64
	TimebaseDen  int64
	StartPTS     float64
	NbFrames     int64
	AvgFrameRate string
	HasEditList  bool
}

// Probe runs ffprobe and returns the container-level streams.
func Probe(ctx context.Context, path string) ([]StreamInfo, error) {
	_, ffprobe, err := Binaries()
	if err != nil {
		return nil, err
	}
	args := []string{
		"-v", "error", "-print_format", "json",
		"-show_streams", "-show_format", path,
	}
	out, err := run(ctx, ffprobe, args...)
	if err != nil {
		return nil, err
	}
	return parseProbe(out)
}

type probeJSON struct {
	Streams []struct {
		Index        int    `json:"index"`
		CodecType    string `json:"codec_type"`
		CodecName    string `json:"codec_name"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		TimeBase     string `json:"time_base"`
		StartTime    string `json:"start_time"`
		NbFrames     string `json:"nb_frames"`
		AvgFrameRate string `json:"avg_frame_rate"`
	} `json:"streams"`
}

func parseProbe(data []byte) ([]StreamInfo, error) {
	var pj probeJSON
	if err := json.Unmarshal(data, &pj); err != nil {
		return nil, fmt.Errorf("parse ffprobe output: %w", err)
	}
	out := make([]StreamInfo, 0, len(pj.Streams))
	for _, s := range pj.Streams {
		num, den := parseFraction(s.TimeBase)
		start, _ := strconv.ParseFloat(s.StartTime, 64)
		frames, _ := strconv.ParseInt(s.NbFrames, 10, 64)
		out = append(out, StreamInfo{
			Index: s.Index, CodecType: s.CodecType, CodecName: s.CodecName,
			Width: s.Width, Height: s.Height, TimebaseNum: num, TimebaseDen: den,
			StartPTS: start, NbFrames: frames, AvgFrameRate: s.AvgFrameRate,
		})
	}
	return out, nil
}

func parseFraction(s string) (int64, int64) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return 0, 1
	}
	n, _ := strconv.ParseInt(parts[0], 10, 64)
	d, _ := strconv.ParseInt(parts[1], 10, 64)
	if d == 0 {
		return 0, 1
	}
	return n, d
}
