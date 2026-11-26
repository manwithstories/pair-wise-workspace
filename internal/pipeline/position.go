package pipeline

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"relaycheck/internal/clock"
	"relaycheck/internal/mapping"
	"relaycheck/internal/provenance"
	"relaycheck/internal/recovery"
	"relaycheck/internal/remux"
)

// ErrInterrupted marks a planned, resumable stop (exit code 3).
var ErrInterrupted = errors.New("run interrupted after committed chunk; rerun with --resume")

// Config configures one relay run.
type Config struct {
	Input     string
	Output    string
	IndexPath string
	Chunk     int
	Workers   int
	MaxMemMB  int
	StopAfter int
	Resume    bool
}

func (c *Config) defaults() {
	if c.Chunk <= 0 {
		c.Chunk = 32
	}
	if c.Workers <= 0 {
		c.Workers = 4
	}
	if c.MaxMemMB <= 0 {
		c.MaxMemMB = 64
	}
}

type runner struct {
	cfg           Config
	mapper        *mapping.Mapper
	timelines     map[uint32]*clock.Timeline
	lastDur       map[uint32]int64
	unrecoverable []string
}

// restore reinstates the timeline checkpoint of the highest committed
// chunk so a resumed run continues on the exact shared timebase.
func (r *runner) restore(idx *recovery.Index) {
	maxC := 0
	for c := range idx.Chunks {
		if c > maxC {
			maxC = c
		}
	}
	if maxC == 0 {
		return
	}
	rec := idx.Chunks[maxC]
	for k, v := range rec.Next {
		if tid, err := strconv.Atoi(k); err == nil {
			if tl, ok := r.timelines[uint32(tid)]; ok {
				tl.Restore(v)
			}
		}
	}
	for k, v := range rec.Dur {
		if tid, err := strconv.Atoi(k); err == nil {
			r.lastDur[uint32(tid)] = v
		}
	}
}

func (r *runner) stateMaps() (map[string]int64, map[string]int64) {
	next := map[string]int64{}
	for tid, tl := range r.timelines {
		if n, ok := tl.Next(); ok {
			next[strconv.Itoa(int(tid))] = n
		}
	}
	dur := map[string]int64{}
	for tid, d := range r.lastDur {
		dur[strconv.Itoa(int(tid))] = d
	}
	return next, dur
}

// skipTimelines accounts for a position whose samples are dropped. When no
// duration estimate exists the loss is unrecoverable and must be reported.
func (r *runner) skipTimelines(pos uint32, kind clock.FaultKind, tr *provenance.SegmentTrace) {
	ok := true
	for tid, tl := range r.timelines {
		d, has := r.lastDur[tid]
		if !has {
			ok = false
			continue
		}
		tl.Skip(d)
	}
	if !ok {
		msg := fmt.Sprintf("unrecoverable %s at seq %d: no duration estimate", kind, pos)
		tr.Repairs = append(tr.Repairs, msg)
		r.unrecoverable = append(r.unrecoverable, msg)
	}
}

func (r *runner) processPosition(pos uint32, prs []*parsedFile) ([]provenance.SegmentTrace, error) {
	sort.Slice(prs, func(i, j int) bool { return prs[i].file.dup < prs[j].file.dup })
	canonical := prs[0]
	tr := provenance.SegmentTrace{
		Seq: pos, File: canonical.file.name, InputSHA: canonical.sha,
		LatencyMs: float64(time.Since(canonical.start).Microseconds()) / 1000.0,
	}
	var traces []provenance.SegmentTrace
	quarantine := func(reason string) ([]provenance.SegmentTrace, error) {
		tr.Decision = provenance.Quarantined
		tr.Repairs = append(tr.Repairs, reason)
		r.skipTimelines(pos, clock.FaultTruncate, &tr)
		return append(traces, tr), nil
	}
	if canonical.fault != nil {
		return quarantine(canonical.fault.String())
	}
	if canonical.seg.Sequence != pos {
		tr.Repairs = append(tr.Repairs, fmt.Sprintf(
			"mfhd gap: expected %d got %d; renumbered without fabricating samples",
			pos, canonical.seg.Sequence))
	}
	var frags []remux.OutFrag
	var tts []provenance.TrackTrace
	type adv struct {
		tl  *clock.Timeline
		dur int64
		tid uint32
	}
	var advances []adv
	for ti := range canonical.seg.Traf {
		tf := &canonical.seg.Traf[ti]
		track, mf := r.mapper.Lookup(tf.TrackID)
		if mf != nil {
			mf.Seq = pos
			return quarantine(mf.String())
		}
		tl := r.timelines[track.OutID]
		dec := tl.ResolveTFDT(tf.HasTFDT, tf.TFDT)
		if dec.Undetermined {
			return quarantine(fmt.Sprintf("track %d: %s", tf.TrackID, dec.Reason))
		}
		if dec.Repaired {
			tr.Repairs = append(tr.Repairs, fmt.Sprintf("track %d: %s (tfdt %d -> %d)",
				tf.TrackID, dec.Reason, dec.Original, dec.Value))
		}
		samples := canonical.samples[ti]
		outs := make([]remux.OutSample, len(samples))
		parts := make([][]byte, 0, len(samples))
		var dur int64
		for i, s := range samples {
			outs[i] = remux.OutSample{Duration: s.Duration, Size: s.Size, Flags: s.Flags, CTO: s.CTO, Data: s.Data}
			parts = append(parts, s.Data)
			dur += int64(s.Duration)
		}
		inHash := remux.HashPayload(parts)
		frags = append(frags, remux.OutFrag{TrackID: track.OutID, TFDT: dec.Value, Samples: outs})
		tts = append(tts, provenance.TrackTrace{
			TrackID: track.OutID, Samples: len(samples), FirstDTS: int64(dec.Value),
			Duration: dur, PayloadSHA: hex.EncodeToString(inHash[:]),
		})
		advances = append(advances, adv{tl: tl, dur: dur, tid: track.OutID})
	}
	hashes, err := r.writeSegment(pos, frags)
	if err != nil {
		return nil, fmt.Errorf("write interrupted at seq %d: %w", pos, err)
	}
	for i := range tts {
		out := hashes[tts[i].TrackID]
		tts[i].OutputSHA = hex.EncodeToString(out[:])
	}
	for _, a := range advances {
		a.tl.Advance(a.dur)
		r.lastDur[a.tid] = a.dur
	}
	tr.Decision = provenance.Accepted
	tr.Tracks = tts
	traces = append(traces, tr)
	for _, pr := range prs[1:] {
		dt := provenance.SegmentTrace{Seq: pos, File: pr.file.name, InputSHA: pr.sha}
		if pr.sha == canonical.sha {
			dt.Decision = provenance.Duplicate
			dt.Repairs = []string{"identical content digest; skipped"}
		} else {
			dt.Decision = provenance.Quarantined
			dt.Repairs = []string{"duplicate name with conflicting content digest"}
		}
		traces = append(traces, dt)
	}
	return traces, nil
}

func (r *runner) writeSegment(pos uint32, frags []remux.OutFrag) (map[uint32][32]byte, error) {
	tmp, err := os.CreateTemp(r.cfg.Output, ".seg-*")
	if err != nil {
		return nil, err
	}
	hashes, err := remux.MarshalSegment(tmp, pos, frags)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return nil, err
	}
	name := filepath.Join(r.cfg.Output, fmt.Sprintf("seg_%06d.m4s", pos))
	return hashes, os.Rename(tmp.Name(), name)
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".atomic-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
