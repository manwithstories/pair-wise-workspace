package main

// Pipeline stages wiring the library modules together. All inter-stage
// data flows through deterministic files under the workdir so that
// committed stages never need to be re-executed on resume.

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/pion/rtp"

	"medrelay/internal/manifest"
	"medrelay/internal/media"
	"medrelay/internal/normalize"
	"medrelay/internal/timeline"
	"medrelay/internal/transport"
	"medrelay/internal/txn"
)

func inputDir(workdir string) string  { return filepath.Join(workdir, "input") }
func stageDir(workdir, name string) string {
	return filepath.Join(workdir, "stages", name)
}

func writeJSON(dir, name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o644)
}

func readJSON(dir, name string, v any) error {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// ---- shared JSON shapes (field order is the on-disk order) ----

type segInfo struct {
	URI        string  `json:"uri"`
	Seq        uint64  `json:"seq"`
	StartMs    int64   `json:"start_ms"`
	DurationMs int64   `json:"duration_ms"`
	Frames     int     `json:"frames"`
}

type errInfo struct {
	Segment string `json:"segment"`
	Code    string `json:"code"`
	StartMs int64  `json:"start_ms"`
	EndMs   int64  `json:"end_ms"`
	Msg     string `json:"msg"`
}

type manifestOut struct {
	Segments []segInfo `json:"segments"`
}
type quarantineOut struct {
	Items []errInfo `json:"items"`
}

type transportOut struct {
	Received  int `json:"received"`
	Lost      int `json:"lost"`
	Recovered int `json:"recovered"`
	Gaps      []struct {
		FromSeq uint16 `json:"from_seq"`
		ToSeq   uint16 `json:"to_seq"`
		StartTS uint32 `json:"start_ts"`
		EndTS   uint32 `json:"end_ts"`
	} `json:"gaps"`
}

type avsyncOut struct {
	Pairs int     `json:"pairs"`
	P95Ms float64 `json:"p95_ms"`
	MaxMs float64 `json:"max_ms"`
}

type summaryOut struct {
	AVSyncErrorMs      float64  `json:"av_sync_error_ms"`
	PacketRecoveryRate float64  `json:"packet_recovery_rate"`
	FirstErrorSegment  string   `json:"first_error_segment"`
	Quarantined        []string `json:"quarantined"`
	SilenceIntervals   int      `json:"silence_intervals"`
	DuplicateWindows   int      `json:"duplicate_windows"`
	UnrecoverableGaps  int      `json:"unrecoverable_gaps"`
}

func p95(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	i := (95*len(xs) + 99) / 100 // ceil(0.95*n)
	return xs[i-1]
}

// ---- stages ----

func stageManifest(workdir string) txn.Stage {
	return txn.Stage{Name: "manifest", Run: func(dir string) error {
		in := inputDir(workdir)
		f, err := os.Open(filepath.Join(in, "playlist.m3u8"))
		if err != nil {
			return err
		}
		segs, err := manifest.ParseHLS(f)
		f.Close()
		if err != nil {
			return err
		}
		mf, err := os.Open(filepath.Join(in, "manifest.mpd"))
		if err != nil {
			return err
		}
		if _, err := manifest.ParseDASH(mf); err != nil {
			mf.Close()
			return err
		}
		mf.Close()
		out := manifestOut{}
		quar := quarantineOut{}
		var video []media.Frame
		for _, s := range segs {
			endMs := s.StartMs + int64(s.Duration*1000+0.5)
			frames, err := readSegmentFile(filepath.Join(in, s.URI), s.URI, s.StartMs, endMs)
			if err != nil {
				if te, ok := err.(*timeline.Error); ok {
					quar.Items = append(quar.Items, errInfo{
						Segment: te.Segment, Code: string(te.Code),
						StartMs: te.StartMs, EndMs: te.EndMs, Msg: te.Msg,
					})
					continue // isolate the segment, keep going
				}
				return err
			}
			video = append(video, frames...)
			out.Segments = append(out.Segments, segInfo{
				URI: s.URI, Seq: s.Seq, StartMs: s.StartMs,
				DurationMs: endMs - s.StartMs, Frames: len(frames),
			})
		}
		if err := writeFramesFile(filepath.Join(dir, "video.frames"), video); err != nil {
			return err
		}
		if err := writeJSON(dir, "segments.json", out); err != nil {
			return err
		}
		return writeJSON(dir, "quarantine.json", quar)
	}}
}

func stageTransport(workdir string) txn.Stage {
	return txn.Stage{Name: "transport", Run: func(dir string) error {
		raw, err := os.ReadFile(filepath.Join(inputDir(workdir), "rtp.stream"))
		if err != nil {
			return err
		}
		r := transport.NewReorderer(transport.Config{MaxQueue: 64, RedundancyStride: redStride})
		var frames []media.Frame
		emit := func(pkts []*rtp.Packet) {
			for _, p := range pkts {
				frames = append(frames, media.Frame{
					Kind: 'a', PTS: int64(p.Timestamp), Dur: audioFrameTicks,
					TS: timeline.Audio, Data: transport.MainPayload(p.Payload),
				})
			}
		}
		for off := 0; off+2 <= len(raw); {
			n := int(binary.BigEndian.Uint16(raw[off : off+2]))
			off += 2
			if off+n > len(raw) {
				return fmt.Errorf("rtp.stream: truncated packet record")
			}
			var p rtp.Packet
			if err := p.Unmarshal(raw[off : off+n]); err != nil {
				return err
			}
			off += n
			em, err := r.Push(&p)
			if err != nil {
				return err
			}
			emit(em)
		}
		emit(r.Drain())
		st := r.Stats()
		out := transportOut{Received: st.Received, Lost: st.Lost, Recovered: st.Recovered}
		for _, g := range r.Gaps() {
			out.Gaps = append(out.Gaps, struct {
				FromSeq uint16 `json:"from_seq"`
				ToSeq   uint16 `json:"to_seq"`
				StartTS uint32 `json:"start_ts"`
				EndTS   uint32 `json:"end_ts"`
			}{g.FromSeq, g.ToSeq, g.StartTS, g.EndTS})
		}
		if err := writeFramesFile(filepath.Join(dir, "audio.frames"), frames); err != nil {
			return err
		}
		return writeJSON(dir, "transport.json", out)
	}}
}

func stageNormalize(workdir string) txn.Stage {
	return txn.Stage{Name: "normalize", Run: func(dir string) error {
		var meta struct {
			Audio normalize.AudioMeta `json:"audio"`
			Color normalize.ColorMeta `json:"color"`
		}
		if err := readJSON(inputDir(workdir), "meta.json", &meta); err != nil {
			return err
		}
		na, err := normalize.NormalizeAudio(meta.Audio)
		if err != nil {
			return err
		}
		var segs manifestOut
		if err := readJSON(stageDir(workdir, "manifest"), "segments.json", &segs); err != nil {
			return err
		}
		var colorQuar []errInfo
		for _, s := range segs.Segments {
			q, err := normalize.ReconcileColor(meta.Color, meta.Color, s.URI, s.StartMs, s.StartMs+s.DurationMs)
			if q {
				colorQuar = append(colorQuar, errInfo{
					Segment: s.URI, Code: string(timeline.ErrColorConflict),
					StartMs: s.StartMs, EndMs: s.StartMs + s.DurationMs, Msg: err.Error(),
				})
			}
		}
		return writeJSON(dir, "params.json", map[string]any{
			"resample_num": na.ResampleNum, "resample_den": na.ResampleDen,
			"channels": na.Channels, "layout": na.Layout,
			"color_quarantined": colorQuar,
		})
	}}
}

func stageSplit(workdir string) txn.Stage {
	return txn.Stage{Name: "split", Run: func(dir string) error {
		var segs manifestOut
		if err := readJSON(stageDir(workdir, "manifest"), "segments.json", &segs); err != nil {
			return err
		}
		video, err := readFramesFile(filepath.Join(stageDir(workdir, "manifest"), "video.frames"))
		if err != nil {
			return err
		}
		audio, err := readFramesFile(filepath.Join(stageDir(workdir, "transport"), "audio.frames"))
		if err != nil {
			return err
		}
		all := append(append([]media.Frame{}, video...), audio...)
		sort.Slice(all, func(i, j int) bool {
			a, b := timeline.ToMillis(all[i].PTS, all[i].TS), timeline.ToMillis(all[j].PTS, all[j].TS)
			if a != b {
				return a < b
			}
			return all[i].Kind < all[j].Kind
		})
		var cuts []int64
		for _, s := range segs.Segments {
			if s.StartMs > 0 {
				cuts = append(cuts, s.StartMs)
			}
		}
		cutSegs, err := media.Split(all, cuts)
		if err != nil {
			return err
		}
		// AV sync: nearest audio frame for each video frame.
		var diffs []float64
		ai := 0
		for _, v := range video {
			vm := timeline.ToMillisExact(v.PTS, v.TS)
			for ai+1 < len(audio) &&
				timeline.ToMillisExact(audio[ai+1].PTS, audio[ai+1].TS) <= vm {
				ai++
			}
			best := timeline.AbsDiffMillis(v.PTS, v.TS, audio[ai].PTS, audio[ai].TS)
			if ai+1 < len(audio) {
				if d := timeline.AbsDiffMillis(v.PTS, v.TS, audio[ai+1].PTS, audio[ai+1].TS); d < best {
					best = d
				}
			}
			diffs = append(diffs, best)
		}
		max := 0.0
		for _, d := range diffs {
			if d > max {
				max = d
			}
		}
		if err := writeJSON(dir, "avsync.json", avsyncOut{Pairs: len(diffs), P95Ms: p95(diffs), MaxMs: max}); err != nil {
			return err
		}
		type cutInfo struct {
			StartMs int64 `json:"start_ms"`
			EndMs   int64 `json:"end_ms"`
			Frames  int   `json:"frames"`
		}
		var ci []cutInfo
		for _, s := range cutSegs {
			ci = append(ci, cutInfo{s.StartMs, s.EndMs, len(s.Frames)})
		}
		return writeJSON(dir, "cuts.json", map[string]any{"segments": ci})
	}}
}

func stageSilence(workdir string) txn.Stage {
	return txn.Stage{Name: "silence", Run: func(dir string) error {
		audio, err := readFramesFile(filepath.Join(stageDir(workdir, "transport"), "audio.frames"))
		if err != nil {
			return err
		}
		n := len(audio) * audioFrameTicks
		chs := [][]int16{make([]int16, 0, n), make([]int16, 0, n)}
		for _, f := range audio {
			for i := 0; i+3 < len(f.Data); i += 4 {
				chs[0] = append(chs[0], int16(binary.LittleEndian.Uint16(f.Data[i:])))
				chs[1] = append(chs[1], int16(binary.LittleEndian.Uint16(f.Data[i+2:])))
			}
		}
		intervals := media.DetectSilence(chs, audioRate, 100, 200)
		return writeJSON(dir, "silence.json", map[string]any{"intervals": intervals})
	}}
}

func stageFingerprint(workdir string) txn.Stage {
	return txn.Stage{Name: "fingerprint", Run: func(dir string) error {
		var segs manifestOut
		if err := readJSON(stageDir(workdir, "manifest"), "segments.json", &segs); err != nil {
			return err
		}
		audio, err := readFramesFile(filepath.Join(stageDir(workdir, "transport"), "audio.frames"))
		if err != nil {
			return err
		}
		d := media.NewDeduper(1000)
		type dup struct {
			Source  string `json:"source"`
			StartMs int64  `json:"start_ms"`
			DupOf   string `json:"dup_of"`
		}
		var dups []dup
		var reasons []string
		for _, s := range segs.Segments {
			rep := d.Process(s.URI, audio, s.StartMs, s.StartMs+s.DurationMs)
			for _, x := range rep.Dups {
				dups = append(dups, dup{x.Window.Source, x.Window.StartMs, x.DupOf.Source})
			}
			reasons = append(reasons, rep.Reasons...)
		}
		return writeJSON(dir, "dedup.json", map[string]any{"dups": dups, "reasons": reasons})
	}}
}

func stageReport(workdir string) txn.Stage {
	return txn.Stage{Name: "report", Run: func(dir string) error {
		var quar quarantineOut
		if err := readJSON(stageDir(workdir, "manifest"), "quarantine.json", &quar); err != nil {
			return err
		}
		var tr transportOut
		if err := readJSON(stageDir(workdir, "transport"), "transport.json", &tr); err != nil {
			return err
		}
		var av avsyncOut
		if err := readJSON(stageDir(workdir, "split"), "avsync.json", &av); err != nil {
			return err
		}
		var sil struct {
			Intervals []media.Interval `json:"intervals"`
		}
		if err := readJSON(stageDir(workdir, "silence"), "silence.json", &sil); err != nil {
			return err
		}
		var dedup struct {
			Dups []json.RawMessage `json:"dups"`
		}
		if err := readJSON(stageDir(workdir, "fingerprint"), "dedup.json", &dedup); err != nil {
			return err
		}
		sum := summaryOut{
			AVSyncErrorMs:      av.P95Ms,
			PacketRecoveryRate: transport.Stats{Lost: tr.Lost, Recovered: tr.Recovered}.RecoveryRate(),
			SilenceIntervals:   len(sil.Intervals),
			DuplicateWindows:   len(dedup.Dups),
			UnrecoverableGaps:  len(tr.Gaps),
		}
		for _, q := range quar.Items {
			if sum.FirstErrorSegment == "" {
				sum.FirstErrorSegment = q.Segment
			}
			sum.Quarantined = append(sum.Quarantined, q.Segment)
		}
		return writeJSON(dir, "summary.json", sum)
	}}
}

func pipelineStages(workdir string) []txn.Stage {
	return []txn.Stage{
		stageManifest(workdir),
		stageTransport(workdir),
		stageNormalize(workdir),
		stageSplit(workdir),
		stageSilence(workdir),
		stageFingerprint(workdir),
		stageReport(workdir),
	}
}
