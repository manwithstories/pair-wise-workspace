package mp4

import (
	"encoding/binary"
	"io"

	"github.com/fashionweek/mediad/internal/clock"
	"github.com/fashionweek/mediad/internal/media"
)

func parseSampleTables(ra io.ReaderAt, stbl Box, tr *TrackIndex, hdr *Header) error {
	tables := map[string][]byte{}
	err := Walk(ra, stbl.Payload, stbl.ContentEnd(), func(b Box) error {
		switch b.Type {
		case "stts", "stss", "ctts", "stsc", "stco", "co64", "stsz":
			buf, err := ReadPayload(ra, b, 64<<20)
			if err != nil {
				return err
			}
			tables[b.Type] = buf
		}
		return nil
	})
	if err != nil {
		return err
	}
	base := stbl.Offset
	dts, durations, n, err := decodeSTTS(tables["stts"], tr.Timescale, base)
	if err != nil {
		return err
	}
	offsets, sizes, err := decodeChunks(tables, n, hdr, base)
	if err != nil {
		return err
	}
	sync, err := decodeSTSS(tables["stss"], n, base)
	if err != nil {
		return err
	}
	cto, _, err := decodeCTTS(tables["ctts"], n, base)
	if err != nil {
		return err
	}
	den := int64(tr.Timescale)
	samples := make([]Sample, n)
	for i := 0; i < n; i++ {
		off, err := cto[i].Mul(1, den)
		if err != nil {
			return media.New(media.PhaseIndex, media.KindOverflow, "", base,
				"composition offset not representable").Wrap(err)
		}
		pts, err := dts[i].Add(off)
		if err != nil {
			return media.New(media.PhaseIndex, media.KindOverflow, "", base,
				"composition timestamp overflow").Wrap(err)
		}
		samples[i] = Sample{
			Index: i, Offset: offsets[i], Size: sizes[i],
			DTS: dts[i], PTS: pts, Duration: durations[i], Sync: sync[i],
		}
		_ = den
	}
	if err := validateTiming(samples, base); err != nil {
		return err
	}
	tr.Samples = samples
	order := presentationOrder(samples)
	rank := make([]int, len(samples))
	for rankPos, decodeIdx := range order {
		rank[decodeIdx] = rankPos
	}
	maxReorder := 0
	for i := range samples {
		d := rank[i] - i
		if d < 0 {
			d = -d
		}
		if d > maxReorder {
			maxReorder = d
		}
	}
	tr.LargestReorder = maxReorder
	return nil
}

// validateTiming rejects zero-duration runs, non-monotonic DTS and PTS
// corruption. A valid B-frame stream may reorder PTS relative to DTS, but the
// reorder window is bounded; a sample whose presentation position implies a
// window larger than maxReorderWindow is treated as PTS rollback corruption.
func validateTiming(samples []Sample, base int64) error {
	if len(samples) == 0 {
		return media.New(media.PhaseIndex, media.KindTruncated, "", base,
			"track contains zero samples")
	}
	for i := 1; i < len(samples); i++ {
		if samples[i].Duration.Num == 0 {
			return media.New(media.PhaseIndex, media.KindBadTimebase, "", base,
				"zero-duration sample detected").AtTime(samples[i].DTS)
		}
		if samples[i].DTS.Cmp(samples[i-1].DTS) <= 0 {
			return media.New(media.PhaseIndex, media.KindPTSRollback, "", base,
				"decode timestamps are not strictly increasing").AtTime(samples[i].DTS)
		}
	}
	order := presentationOrder(samples)
	rank := make([]int, len(samples))
	for pos, decodeIdx := range order {
		rank[decodeIdx] = pos
	}
	seen := make(map[int64]struct{}, len(samples))
	for pos, decodeIdx := range order {
		pts := samples[decodeIdx].PTS
		// A presentation timestamp before the track origin is a PTS rollback:
		// legitimate composition offsets never precede the first media time.
		if pts.Num < 0 {
			return media.New(media.PhaseIndex, media.KindPTSRollback, "", base,
				"presentation timestamp precedes track origin (PTS rollback)").
				AtTime(pts)
		}
		key := pts.Num*1_000_003 + pts.Den
		if _, dup := seen[key]; dup {
			return media.New(media.PhaseIndex, media.KindPTSRollback, "", base,
				"duplicate presentation timestamp after composition offsets").AtTime(pts)
		}
		seen[key] = struct{}{}
		window := pos - decodeIdx
		if window < 0 {
			window = -window
		}
		if window > maxReorderWindow {
			return media.New(media.PhaseIndex, media.KindPTSRollback, "", base,
				"presentation reorder window exceeds bounded frame reordering").
				AtTime(samples[decodeIdx].PTS)
		}
	}
	return nil
}

func decodeSTTS(buf []byte, ts uint32, base int64) ([]clock.Rat, []clock.Rat, int, error) {
	if len(buf) == 0 {
		return nil, nil, 0, media.New(media.PhaseIndex, media.KindTruncated, "", base, "stts box missing")
	}
	_, _, rest := FullBoxVersion(buf)
	if len(rest) < 4 {
		return nil, nil, 0, media.New(media.PhaseScan, media.KindTruncated, "", base, "stts count missing")
	}
	entries := int(binary.BigEndian.Uint32(rest[:4]))
	at := 4
	den := int64(ts)
	total := 0
	type run struct{ count, delta int64 }
	runs := make([]run, 0, entries)
	for i := 0; i < entries; i++ {
		c, a1, e := readU32(rest, at, base)
		if e != nil {
			return nil, nil, 0, e
		}
		d, a2, e := readU32(rest, a1, base)
		if e != nil {
			return nil, nil, 0, e
		}
		at = a2
		runs = append(runs, run{int64(c), int64(d)})
		total += int(c)
	}
	dts := make([]clock.Rat, 0, total)
	durs := make([]clock.Rat, 0, total)
	var cur int64
	for _, r := range runs {
		d, err := clock.New(r.delta, den)
		if err != nil {
			return nil, nil, 0, media.New(media.PhaseIndex, media.KindBadTimebase, "", base,
				"stts delta not representable").Wrap(err)
		}
		for j := int64(0); j < r.count; j++ {
			t, err := clock.New(cur, den)
			if err != nil {
				return nil, nil, 0, media.New(media.PhaseIndex, media.KindOverflow, "", base,
					"stts timestamp overflow").Wrap(err)
			}
			dts = append(dts, t)
			durs = append(durs, d)
			cur += r.delta
			if cur < 0 {
				return nil, nil, 0, media.New(media.PhaseIndex, media.KindOverflow, "", base,
					"stts cumulative timestamp overflow")
			}
		}
	}
	return dts, durs, total, nil
}

func decodeSTSS(buf []byte, n int, base int64) ([]bool, error) {
	sync := make([]bool, n)
	if len(buf) == 0 {
		// No stss: every sample is a sync sample (common for plain AVC clips
		// without B-frames and for audio-only tracks).
		for i := range sync {
			sync[i] = true
		}
		return sync, nil
	}
	_, _, rest := FullBoxVersion(buf)
	if len(rest) < 4 {
		return nil, media.New(media.PhaseScan, media.KindTruncated, "", base, "stss count missing")
	}
	entries := int(binary.BigEndian.Uint32(rest[:4]))
	at := 4
	for i := 0; i < entries; i++ {
		v, a2, e := readU32(rest, at, base)
		if e != nil {
			return nil, e
		}
		at = a2
		if v == 0 || int(v) > n {
			return nil, media.New(media.PhaseIndex, media.KindBounds, "", base,
				"stss sync sample index out of bounds")
		}
		sync[v-1] = true
	}
	return sync, nil
}

func decodeCTTS(buf []byte, n int, base int64) ([]clock.Rat, bool, error) {
	zeros := make([]clock.Rat, n)
	for i := range zeros {
		zeros[i] = clock.MustNew(0, 1)
	}
	if len(buf) == 0 {
		return zeros, false, nil
	}
	version, _, rest := FullBoxVersion(buf)
	if len(rest) < 4 {
		return nil, false, media.New(media.PhaseScan, media.KindTruncated, "", base, "ctts count missing")
	}
	entries := int(binary.BigEndian.Uint32(rest[:4]))
	at := 4
	out := make([]clock.Rat, 0, n)
	for i := 0; i < entries; i++ {
		c, a1, e := readU32(rest, at, base)
		if e != nil {
			return nil, false, e
		}
		var off int64
		if version == 1 {
			v, a2, e2 := readU32(rest, a1, base)
			if e2 != nil {
				return nil, false, e2
			}
			off = int64(int32(v))
			at = a2
		} else {
			v, a2, e2 := readU32(rest, a1, base)
			if e2 != nil {
				return nil, false, e2
			}
			off = int64(v)
			at = a2
		}
		// Denominator is fixed when values are consumed; timescale applied by
		// caller through Mul — here keep raw ticks and scale below.
		for j := uint32(0); j < c; j++ {
			r, err := clock.New(off, 1)
			if err != nil {
				return nil, false, media.New(media.PhaseIndex, media.KindOverflow, "", base,
					"ctts offset overflow").Wrap(err)
			}
			out = append(out, r)
		}
	}
	if len(out) != n {
		return nil, false, media.New(media.PhaseIndex, media.KindBounds, "", base,
			"ctts entry count does not match sample count")
	}
	return out, true, nil
}
