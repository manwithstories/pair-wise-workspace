package scan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"aircheck/internal/clock"
	"aircheck/internal/fault"
)

// Event is one timed metadata item on the shared 90 kHz timeline.
type Event struct {
	Type      string `json:"type"` // chapter, subtitle, id3, emsg, sei
	Track     string `json:"track"`
	Tick      int64  `json:"tick"` // 90 kHz
	DurTicks  int64  `json:"dur_ticks,omitempty"`
	Payload   string `json:"payload,omitempty"`
	SegmentID int    `json:"segment_id"` // assigned by plan, -1 before
}

func execOutput(bin string, args ...string) ([]byte, error) {
	cmd := exec.Command(bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s failed: %v: %s", bin, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func joinPath(root, file string) string {
	if filepath.IsAbs(file) {
		return file
	}
	return filepath.Join(root, file)
}

type ffChapter struct {
	Start     string            `json:"start_time"`
	End       string            `json:"end_time"`
	Tags      map[string]string `json:"tags"`
}

// chaptersOf converts ffprobe chapters into events.
func chaptersOf(fo *ffprobeOut, file string) ([]Event, error) {
	var out []Event
	for i, c := range fo.Chapters {
		start := int64(parseFloat(c.Start) * float64(clock.Timescale))
		end := int64(parseFloat(c.End) * float64(clock.Timescale))
		if err := clock.CheckDuration(end - start, file+":chapter"); err != nil {
			return nil, err
		}
		out = append(out, Event{
			Type: "chapter", Track: file + ":chapter",
			Tick: start, DurTicks: end - start,
			Payload:   fmt.Sprintf("%d:%s", i, c.Tags["title"]),
			SegmentID: -1,
		})
	}
	return out, nil
}

// subtitlesOf decodes the first subtitle stream to SRT and parses cues.
func subtitlesOf(path, file string, durTicks int64) ([]Event, error) {
	hasSub := false
	fo, err := probeFile(path)
	if err != nil {
		return nil, err
	}
	for _, s := range fo.Streams {
		if s.CodecType == "subtitle" {
			hasSub = true
		}
	}
	if !hasSub {
		return nil, nil
	}
	release := acquire()
	defer release()
	raw, err := execOutput("ffmpeg", "-v", "error", "-i", path,
		"-map", "0:s:0", "-f", "srt", "pipe:1")
	if err != nil {
		return nil, fault.New(fault.CodeMalformedEvent, file+":subtitle", -1, false,
			"subtitle decode failed: %v", err)
	}
	return parseSRT(string(raw), file+":subtitle", durTicks)
}

// parseSRT parses cue times onto the 90 kHz base and validates them
// against the rendition duration.
func parseSRT(srt, track string, durTicks int64) ([]Event, error) {
	var out []Event
	blocks := strings.Split(strings.ReplaceAll(srt, "\r\n", "\n"), "\n\n")
	for _, b := range blocks {
		lines := strings.Split(strings.TrimSpace(b), "\n")
		if len(lines) < 2 {
			continue
		}
		var t1, t2 string
		if n, _ := fmt.Sscanf(lines[1], "%s --> %s", &t1, &t2); n != 2 {
			return nil, fault.New(fault.CodeMalformedEvent, track, -1, false,
				"bad cue timing line %q", lines[1])
		}
		start, err := parseSRTTime(t1)
		if err != nil {
			return nil, fault.New(fault.CodeMalformedEvent, track, -1, false, "%v", err)
		}
		end, err := parseSRTTime(t2)
		if err != nil {
			return nil, fault.New(fault.CodeMalformedEvent, track, -1, false, "%v", err)
		}
		if err := clock.CheckDuration(end-start, track); err != nil {
			return nil, err
		}
		if durTicks > 0 && start > durTicks+truncTolerance {
			return nil, fault.New(fault.CodeMalformedEvent, track, start, false,
				"cue starts past media end %s", clock.String(durTicks))
		}
		out = append(out, Event{
			Type: "subtitle", Track: track, Tick: start,
			DurTicks: end - start, Payload: strings.Join(lines[2:], " "),
			SegmentID: -1,
		})
	}
	return out, nil
}

func parseSRTTime(s string) (int64, error) {
	var h, m, sec, ms int
	if _, err := fmt.Sscanf(strings.ReplaceAll(s, ",", "."),
		"%d:%d:%d.%d", &h, &m, &sec, &ms); err != nil {
		return 0, fmt.Errorf("bad srt time %q", s)
	}
	total := ((int64(h)*60+int64(m))*60+int64(sec))*clock.Timescale + int64(ms)*90
	return total, clock.CheckDuration(total, "srt")
}

// sidecarEvents reads the vendor timed-metadata manifest (ID3/emsg/SEI)
// and validates each event against every rendition duration.
func sidecarEvents(root string, rends []Rendition) ([]Event, error) {
	raw, err := os.ReadFile(filepath.Join(root, "events.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Events []struct {
			Type    string `json:"type"`
			TimeMS  int64  `json:"time_ms"`
			Payload string `json:"payload"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fault.New(fault.CodeMalformedEvent, "events.json", -1, false,
			"sidecar undecodable: %v", err)
	}
	var maxDur int64
	for _, r := range rends {
		if r.DurationTicks > maxDur {
			maxDur = r.DurationTicks
		}
	}
	var out []Event
	for _, e := range doc.Events {
		switch e.Type {
		case "id3", "emsg", "sei":
		default:
			return nil, fault.New(fault.CodeMalformedEvent, "events.json", -1, false,
				"unknown event type %q", e.Type)
		}
		tick := e.TimeMS * 90
		if err := clock.CheckDuration(tick, "events.json"); err != nil {
			return nil, err
		}
		if maxDur > 0 && tick > maxDur+truncTolerance {
			return nil, fault.New(fault.CodeMalformedEvent, "events.json", tick, false,
				"%s event past media end %s", e.Type, clock.String(maxDur))
		}
		out = append(out, Event{Type: e.Type, Track: "events.json",
			Tick: tick, Payload: e.Payload, SegmentID: -1})
	}
	return out, nil
}
