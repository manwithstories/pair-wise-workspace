package main

import (
	"fmt"
	"strings"

	"deliver/internal/hls"
	"deliver/internal/timecode"
)

// camSpec describes one synthetic camera feeding the pipeline.
type camSpec struct {
	name   string
	fps    timecode.FPS
	drop   bool
	offset int64 // timecode label offset vs the unified clock, ms
	skew   int64 // av sync skew, ms
}

// defaultCams returns stage, side and roam cameras. With wrap, the roam
// camera's labels start 90 frames before midnight and cross over.
func defaultCams(wrap bool) []camSpec {
	offset := int64(0)
	if wrap {
		offset = -3003 // exactly -90 frames at 30000/1001
	}
	return []camSpec{
		{"stage", timecode.FPS25, false, 0, 0},
		{"side", timecode.FPS25, false, 0, 7},
		{"roam", timecode.FPS2997, true, offset, 13},
	}
}

func srtTS(ms int64) string {
	return fmt.Sprintf("%02d:%02d:%02d,%03d",
		ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// genSRT builds deterministic subtitles; faults adds a bad timestamp
// row, a reversed record, an overlong record and a truncated tail.
func genSRT(seconds int, faults bool) string {
	var b strings.Builder
	n := 1
	for t := int64(250); t < int64(seconds)*1000; t += 1500 {
		fmt.Fprintf(&b, "%d\n%s --> %s\ncue %d\n\n", n, srtTS(t), srtTS(t+1400), n)
		n++
	}
	if faults {
		b.WriteString("9001\nnot-a-timestamp\nbroken record\n\n")
		fmt.Fprintf(&b, "9002\n%s --> %s\nreversed\n\n", srtTS(9000), srtTS(8000))
		fmt.Fprintf(&b, "9003\n%s --> %s\n%s\n\n",
			srtTS(10000), srtTS(11000), strings.Repeat("x", 5000))
		b.WriteString("9004\n00:00:12,000 -->") // truncated tail
	}
	return b.String()
}

// genEvents synthesizes AV events on the unified timeline, one per
// 500ms slot per camera, with per-camera av skew.
func genEvents(seconds int, cams []camSpec) []hls.Event {
	var evs []hls.Event
	seq := int64(0)
	for t := int64(0); t < int64(seconds)*1000; t += 500 {
		for _, c := range cams {
			evs = append(evs, hls.Event{Seq: seq, Cam: c.name,
				MediaMs: t + c.skew, Bytes: 900 + int(seq%200)})
			seq++
		}
	}
	return evs
}

// calibrate resolves a camera's start label against the unified clock.
func calibrate(c camSpec) (int64, error) {
	label := timecode.Format(timecode.MsToFrames(c.offset, c.fps), c.fps, c.drop)
	f, err := timecode.Parse(label, c.fps, c.drop)
	if err != nil {
		return 0, err
	}
	return timecode.ResolveDaily(f, c.fps, c.drop, 0)
}

// genCheckpoints formats each camera's wrapping timecode labels and
// resolves them back onto the unified timeline, so a midnight wrap in
// the roam camera propagates through timecode into segmentation.
func genCheckpoints(cams []camSpec, seconds int) ([]hls.Event, error) {
	var out []hls.Event
	seq := int64(1_000_000)
	for _, c := range cams {
		off, err := calibrate(c)
		if err != nil {
			return nil, err
		}
		for t := int64(0); t < int64(seconds)*1000; t += 1000 {
			label := timecode.Format(timecode.MsToFrames(off+t, c.fps), c.fps, c.drop)
			f, err := timecode.Parse(label, c.fps, c.drop)
			if err != nil {
				return nil, err
			}
			ms, err := timecode.ResolveDaily(f, c.fps, c.drop, off+t)
			if err != nil {
				return nil, err
			}
			out = append(out, hls.Event{Seq: seq, Cam: c.name, MediaMs: ms - off, Bytes: 64})
			seq++
		}
	}
	return out, nil
}

// injectFaults deterministically drops packets (most recoverable via
// the retry store), duplicates some and swaps adjacent pairs to
// simulate lossy, out-of-order return from the tour venue.
func injectFaults(evs []hls.Event) (stream, retry []hls.Event) {
	for _, e := range evs {
		if e.Seq%23 == 7 { // packet loss
			if (e.Seq/23)%25 != 24 { // >=96% retransmitted
				retry = append(retry, e)
			}
			continue
		}
		stream = append(stream, e)
		if e.Seq%89 == 5 { // duplicate
			stream = append(stream, e)
		}
	}
	for i := 0; i+1 < len(stream); i += 2 { // adjacent swaps = reorder
		if stream[i].Seq%31 == 9 {
			stream[i], stream[i+1] = stream[i+1], stream[i]
		}
	}
	return stream, retry
}
