// Package scan reads a delivery package, probes every rendition with
// ffprobe and builds the track index. It validates durations and packet
// timestamps on the 90 kHz base and reports truncation, negative
// durations and regressive timestamps with track and tick context.
package scan

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"aircheck/internal/clock"
	"aircheck/internal/fault"
)

// sem bounds concurrent ffprobe/ffmpeg processes.
var sem = make(chan struct{}, 2)

func acquire() func() { sem <- struct{}{}; return func() { <-sem } }

// Track describes one elementary stream.
type Track struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"` // video, audio, subtitle, data
	Codec         string `json:"codec"`
	Channels      int    `json:"channels,omitempty"`
	ChannelLayout string `json:"channel_layout,omitempty"`
	SampleRate    int    `json:"sample_rate,omitempty"`
	Width         int    `json:"width,omitempty"`
	Height        int    `json:"height,omitempty"`
	ColorSpace    string `json:"color_space,omitempty"`
	ColorPrim     string `json:"color_primaries,omitempty"`
	ColorTransfer string `json:"color_transfer,omitempty"`
	TimeBase      string `json:"time_base"`
	DurationTicks int64  `json:"duration_ticks"` // 90 kHz
	BitRate       int64  `json:"bit_rate,omitempty"`
	NBFrames      int64  `json:"nb_frames,omitempty"`
}

// Rendition is one delivered bitrate variant.
type Rendition struct {
	Name          string  `json:"name"`
	File          string  `json:"file"`
	TargetBPS     int64   `json:"target_bps"`
	DurationTicks int64   `json:"duration_ticks"`
	Tracks        []Track `json:"tracks"`
}

// Delivery is the vendor manifest shipped inside the package.
type Delivery struct {
	Package    string `json:"package"`
	SegmentMS  int    `json:"segment_ms"`
	Renditions []struct {
		Name      string `json:"name"`
		File      string `json:"file"`
		TargetBPS int64  `json:"target_bps"`
	} `json:"renditions"`
}

// Result is the persisted scan index payload.
type Result struct {
	Package      string      `json:"package"`
	SegmentTicks int64       `json:"segment_ticks"`
	Renditions   []Rendition `json:"renditions"`
	Events       []Event     `json:"events"`
}

type ffprobeStream struct {
	Index         int    `json:"index"`
	CodecType     string `json:"codec_type"`
	CodecName     string `json:"codec_name"`
	Channels      int    `json:"channels"`
	ChannelLayout string `json:"channel_layout"`
	SampleRate    string `json:"sample_rate"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	ColorSpace    string `json:"color_space"`
	ColorPrim     string `json:"color_primaries"`
	ColorTransfer string `json:"color_transfer"`
	TimeBase      string `json:"time_base"`
	Duration      string `json:"duration"`
	BitRate       string `json:"bit_rate"`
	NBFrames      string `json:"nb_frames"`
}

type ffprobeOut struct {
	Streams  []ffprobeStream `json:"streams"`
	Format   struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
	} `json:"format"`
	Chapters []ffChapter `json:"chapters"`
}

func runJSON(bin string, args []string, out any) error {
	release := acquire()
	defer release()
	cmd := exec.Command(bin, args...)
	stdout, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("%s failed: %v: %s", bin, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return err
	}
	return json.Unmarshal(stdout, out)
}

func parseTimeBase(tb string) (int64, error) {
	var num, den int64
	if _, err := fmt.Sscanf(tb, "%d/%d", &num, &den); err != nil || num != 1 || den <= 0 {
		return 0, fmt.Errorf("unparsable time_base %q", tb)
	}
	return den, nil
}

func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

// probeFile runs ffprobe over one rendition file.
func probeFile(path string) (*ffprobeOut, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fault.New(fault.CodeMissingTrack, filepath.Base(path), -1, false,
			"rendition file missing: %v", err)
	}
	var out ffprobeOut
	err := runJSON("ffprobe", []string{
		"-v", "error", "-show_streams", "-show_format", "-show_chapters",
		"-of", "json", path,
	}, &out)
	if err != nil {
		return nil, fault.New(fault.CodeTruncated, filepath.Base(path), -1, false,
			"ffprobe failed (truncated or unreadable media): %v", err)
	}
	return &out, nil
}

// toTrack converts and validates one probed stream.
func toTrack(s ffprobeStream, file string) (Track, error) {
	id := fmt.Sprintf("%s:%d", file, s.Index)
	t := Track{
		ID: id, Kind: s.CodecType, Codec: s.CodecName,
		Channels: s.Channels, ChannelLayout: s.ChannelLayout,
		Width: s.Width, Height: s.Height,
		ColorSpace: s.ColorSpace, ColorPrim: s.ColorPrim, ColorTransfer: s.ColorTransfer,
		TimeBase: s.TimeBase,
		BitRate:  int64(parseFloat(s.BitRate)),
		NBFrames: int64(parseFloat(s.NBFrames)),
	}
	if sr, err := strconv.Atoi(s.SampleRate); err == nil {
		t.SampleRate = sr
	}
	if s.Duration != "" {
		d := parseFloat(s.Duration)
		ticks := int64(d*float64(clock.Timescale) + 0.5)
		if err := clock.CheckDuration(ticks, id); err != nil {
			return t, err
		}
		t.DurationTicks = ticks
	}
	return t, nil
}
