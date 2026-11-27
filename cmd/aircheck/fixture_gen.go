package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
)

// writeSubtitles emits a deterministic SRT sidecar used as the subtitle
// source stream.
func writeSubtitles(dir string, rng *rand.Rand, durSec int) (string, error) {
	p := filepath.Join(dir, "subs.srt")
	var b []byte
	t := 0.2
	for i := 1; t+0.8 < float64(durSec); i++ {
		end := t + 0.6 + float64(rng.Intn(3))/10
		b = append(b, []byte(fmt.Sprintf("%d\n%s --> %s\nfixture cue %d\n\n",
			i, srtTime(t), srtTime(end), i))...)
		t = end + 0.4
	}
	return p, os.WriteFile(p, b, 0o644)
}

func srtTime(sec float64) string {
	ms := int64(sec * 1000)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// writeChapters emits an ffmetadata chapter file.
func writeChapters(dir string, rng *rand.Rand, durSec int) (string, error) {
	p := filepath.Join(dir, "chapters.txt")
	b := ";FFMETADATA1\n"
	n := 2 + rng.Intn(2) // 2-3 chapters
	step := durSec * 1000 / n
	for i := 0; i < n; i++ {
		b += fmt.Sprintf("[CHAPTER]\nTIMEBASE=1/1000\nSTART=%d\nEND=%d\ntitle=Chapter %d\n",
			i*step, (i+1)*step, i+1)
	}
	return p, os.WriteFile(p, []byte(b), 0o644)
}

// writeEvents emits the vendor timed-metadata sidecar (ID3/emsg/SEI).
func writeEvents(dir string, rng *rand.Rand, durSec int) error {
	type ev struct {
		Type    string `json:"type"`
		TimeMS  int64  `json:"time_ms"`
		Payload string `json:"payload"`
	}
	kinds := []string{"id3", "emsg", "sei"}
	var evs []ev
	for i := 0; i < 5; i++ {
		ms := int64(rng.Intn(durSec*1000-500)) + 100
		evs = append(evs, ev{Type: kinds[i%3], TimeMS: ms,
			Payload: fmt.Sprintf("evt-%d", i)})
	}
	raw, err := json.MarshalIndent(map[string]any{"events": evs}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "events.json"), raw, 0o644)
}

// writeDelivery emits the vendor manifest with declared bitrates probed
// from the generated files so the declared targets match the delivery.
func writeDelivery(dir string, specs []fixtureSpec) error {
	type rend struct {
		Name      string `json:"name"`
		File      string `json:"file"`
		TargetBPS int64  `json:"target_bps"`
	}
	names := []string{"hi", "lo"}
	d := struct {
		Package    string `json:"package"`
		SegmentMS  int    `json:"segment_ms"`
		Renditions []rend `json:"renditions"`
	}{Package: "fixture", SegmentMS: 2000}
	for i, s := range specs {
		bps, err := probeBitrate(filepath.Join(dir, s.name))
		if err != nil {
			return err
		}
		d.Renditions = append(d.Renditions, rend{Name: names[i], File: s.name, TargetBPS: bps})
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "delivery.json"), raw, 0o644)
}
