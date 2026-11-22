// Package demo runs the mediasync demonstration pipeline: generate ->
// parse -> repair -> align -> ladder -> WAL commit -> verify.
package demo

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"mediasync/internal/align"
	"mediasync/internal/clock"
	"mediasync/internal/fixture"
	"mediasync/internal/manifest"
	"mediasync/internal/rtprepair"
	"mediasync/internal/store"
	"mediasync/internal/subtitle"
	"mediasync/internal/verify"
)

// Exit codes.
const (
	ExitOK       = 0 // verification passed
	ExitUsage    = 2 // input or boundary error
	ExitRecovery = 3 // recovery or hash verification failure
)

// Pipeline executes the full demo flow and returns metric lines plus the
// process exit code.
func Pipeline(seed int64, tb clock.Timebase, fpsStr string, bitrates []int,
	faults map[string]bool, crashAt, outDir string) ([]string, int) {

	sample := fixture.Generate(seed, tb, faults)

	// Subtitles: SRT cues carry the venue wall clock (base 23:54:00, crosses
	// midnight); VTT cues are already material-relative.
	srtOK := subtitle.ParseSRT(strings.NewReader(sample.ValidSRT), tb)
	srtBad := subtitle.ParseSRT(strings.NewReader(sample.CorruptSRT), tb)
	vtt := subtitle.ParseVTT(strings.NewReader(sample.ValidVTT), tb)
	rejected := len(srtOK.Rejected) + len(srtBad.Rejected) + len(vtt.Rejected)
	records := toMaterialTime(tb, srtOK.Records)
	records = append(records, vtt.Records...)

	// RTP repair: dedup, reorder, mark losses (never fabricate).
	win := rtprepair.New(16)
	var delivered []int64
	var lostList []int64
	collect := func(items []rtprepair.Item) {
		for _, it := range items {
			if it.Packet != nil {
				delivered = append(delivered, int64(binary.LittleEndian.Uint64(it.Packet.Payload)))
			}
			for _, s := range it.Missing {
				lostList = append(lostList, int64(s))
			}
		}
	}
	for _, p := range sample.Packets {
		collect(win.Push(p))
	}
	collect(win.Flush())
	lostSet := map[int64]bool{}
	for _, f := range lostList {
		lostSet[f] = true
	}
	gaps := coalesce(lostList)

	// Segment on keyframe boundaries; lost keyframes lose random access.
	segs := align.Cut(sample.TotalFrames, sample.SegmentFrames, sample.Keyframes, lostSet, gaps)

	// Bitrate ladder: reference boundaries plus deterministic ±1-frame
	// jitter on the other rungs, snapped back by Align (max 1 frame).
	canon := make([]int64, len(segs))
	for i, s := range segs {
		canon[i] = s.EndFrame
	}
	reps := []manifest.Rep{{Bitrate: bitrates[0], Boundaries: canon}}
	for i, b := range bitrates[1:] {
		reps = append(reps, manifest.Rep{Bitrate: b, Boundaries: jitter(canon, seed, i)})
	}
	ladder, err := manifest.Align(fpsStr, reps, 1)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, ExitUsage
	}
	initSeg, err := manifest.MarshalInit()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, ExitRecovery
	}

	// Per-segment content and hashes (deterministic for a given seed).
	contents := segmentContents(sample, segs, records)
	hashes := verify.HashSegments(contents)

	// Transactional commit through the WAL, with optional crash recovery.
	st, rollbacks, err := store.Open(outDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, ExitRecovery
	}
	commitArtifact, _ := json.Marshal(map[string]any{
		"timebase": fpsStr, "ladder": ladder, "hashes": hashes,
		"init_sha256": verify.Sum256(initSeg),
	})
	stages := []struct {
		name string
		data []byte
	}{
		{"ingest", mustJSON(map[string]int{"records": len(records), "rejected": rejected})},
		{"reorder", mustJSON(win.Stats)},
		{"align", mustJSON(len(segs))},
		{"manifest", mustJSON(ladder)},
		{"commit", commitArtifact},
	}
	recovered := false
	for _, sg := range stages {
		if crashAt == "commit" && sg.name == "commit" && !st.Done(sg.name) {
			// Crash before the commit record: the uncommitted artifact
			// is rolled back on reopen, then recommitted idempotently.
			if err := st.WriteUncommitted(sg.name, sg.data); err != nil {
				fmt.Fprintln(os.Stderr, err)
				st.Close()
				return nil, ExitRecovery
			}
			st.Close()
			var rb int
			st, rb, err = store.Open(outDir)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return nil, ExitRecovery
			}
			rollbacks += rb
			recovered = true
		}
		if err := st.Commit(sg.name, sg.data); err != nil {
			fmt.Fprintln(os.Stderr, err)
			st.Close()
			return nil, ExitRecovery
		}
	}
	artifactPath := st.ArtifactPath("commit")
	st.Close()

	// Verify committed hashes against recomputed content.
	committed, err := os.ReadFile(artifactPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, ExitRecovery
	}
	var stored struct {
		Hashes []string `json:"hashes"`
	}
	if err := json.Unmarshal(committed, &stored); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, ExitRecovery
	}
	bad := verify.CheckSegments(contents, stored.Hashes)
	for _, b := range bad {
		fmt.Fprintln(os.Stderr, b.Error())
	}

	ra := 0
	for _, s := range segs {
		if s.RandomAccess {
			ra++
		}
	}
	metrics := []string{
		fmt.Sprintf("roundtrip_rate=%.4f", float64(len(delivered))/float64(sample.TotalFrames)),
		fmt.Sprintf("keyframe_hit_rate=%.4f", float64(ra)/float64(len(segs))),
		fmt.Sprintf("segment_hash_mismatch=%d", len(bad)),
		fmt.Sprintf("segments=%d", len(segs)),
		fmt.Sprintf("recovered=%t", recovered),
		fmt.Sprintf("rejected_records=%d", rejected),
		fmt.Sprintf("rollback_count=%d", rollbacks),
	}
	if len(bad) > 0 {
		return metrics, ExitRecovery
	}
	return metrics, ExitOK
}

// toMaterialTime converts wall-clock SRT cue frames to material-relative
// frames, unwrapping cross-midnight timecodes.
func toMaterialTime(tb clock.Timebase, recs []subtitle.Record) []subtitle.Record {
	base := tb.MillisToFrames((23*3600 + 54*60) * 1000) // material starts 23:54:00
	prev := base
	out := make([]subtitle.Record, 0, len(recs))
	for _, r := range recs {
		sAbs := tb.UnwrapDay(prev, r.Start)
		eAbs := tb.UnwrapDay(sAbs, r.End)
		r.Start, r.End = sAbs-base, eAbs-base
		prev = sAbs
		out = append(out, r)
	}
	return out
}

// coalesce merges lost frames into [start,end) ranges.
func coalesce(lost []int64) [][2]int64 {
	if len(lost) == 0 {
		return nil
	}
	sort.Slice(lost, func(i, j int) bool { return lost[i] < lost[j] })
	var out [][2]int64
	start, prev := lost[0], lost[0]
	for _, f := range lost[1:] {
		if f != prev+1 {
			out = append(out, [2]int64{start, prev + 1})
			start = f
		}
		prev = f
	}
	return append(out, [2]int64{start, prev + 1})
}

// jitter returns canon with deterministic ±1-frame offsets (except the
// final boundary) for non-reference rungs.
func jitter(canon []int64, seed int64, rep int) []int64 {
	out := make([]int64, len(canon))
	copy(out, canon)
	for i := 0; i+1 < len(out); i++ {
		h := sha256.Sum256([]byte(fmt.Sprintf("jitter/%d/%d/%d", seed, rep, i)))
		out[i] += int64(h[0]%3) - 1
	}
	return out
}

// segmentContents builds deterministic per-segment content for hashing:
// segment bounds, gaps, overlapping cue indices and delivered frame hashes.
func segmentContents(sample *fixture.Sample, segs []align.Segment, recs []subtitle.Record) [][]byte {
	frameHash := map[int64][32]byte{}
	for _, p := range sample.Packets {
		f := int64(binary.LittleEndian.Uint64(p.Payload))
		frameHash[f] = sha256.Sum256(p.Payload)
	}
	out := make([][]byte, len(segs))
	for i, s := range segs {
		h := sha256.New()
		fmt.Fprintf(h, "seg/%d/%d/%d/%t/", s.Index, s.StartFrame, s.EndFrame, s.RandomAccess)
		for _, g := range s.Gaps {
			fmt.Fprintf(h, "g%d-%d/", g[0], g[1])
		}
		for j, r := range recs {
			if r.Start < s.EndFrame && r.End > s.StartFrame {
				fmt.Fprintf(h, "r%d/", j)
			}
		}
		for f := s.StartFrame; f < s.EndFrame; f++ {
			if fh, ok := frameHash[f]; ok {
				h.Write(fh[:])
			}
		}
		out[i] = h.Sum(nil)
	}
	return out
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
