package fixture

import (
	"encoding/binary"
	"fmt"
	"os"
)

// injectMixed applies the mixed profile's fault programme to the encoded
// representations. The carrier (high) stays complete and correct so the
// exported frame count is exactly the source frame count; faults that isolate
// fragments or shift a level are placed on the reference levels so the report
// still demonstrates every class named in the acceptance contract.
//
//	high : VFR-injected timestamp wrap (unwraps, stays 200 frames),
//	       boundary reference, complete
//	mid  : dropped segment, duplicate segment
//	low  : tfdt regression (isolated), clock drift, truncated mdat,
//	       whole-level boundary shift
func injectMixed(dir string, m *Manifest) error {
	byName := map[string]*Entry{}
	for i := range m.Levels {
		byName[m.Levels[i].Level] = &m.Levels[i]
	}

	// --- high: wrap + drift on the carrier, no fragment loss --------------
	{
		e := byName["high"]
		data, spans, err := load(e.Path)
		if err != nil {
			return err
		}
		if len(spans) < 8 {
			return fmt.Errorf("high representation too short for the mixed programme")
		}
		half := len(spans) / 2
		// Drift first, on true (unwrapped) time: the clock runs 800 ppm fast
		// from the midpoint, so every later fragment is proportionally late.
		const ppm = 800
		if err := RewriteTFDTFunc(data, spans[half:], func(old int64) int64 {
			return old + old*ppm/1_000_000
		}); err != nil {
			return fmt.Errorf("drift injection (high): %w", err)
		}
		// Then slide the whole representation so it crosses the 32-bit
		// boundary: values keep their true (unbounded) magnitude, but the
		// low 32 bits are what a 32-bit reader would store. A v1 field keeps
		// the full value, so to make the wrap observable the second half is
		// rewritten as the low 32 bits alone (the arithmetic a v0 writer
		// performs), and the unwrapper restores the missing window.
		const wrapBase = int64(1) << 32
		if err := RewriteTFDTFunc(data, spans, func(old int64) int64 {
			return old + wrapBase
		}); err != nil {
			return fmt.Errorf("wrap injection (high) offset: %w", err)
		}
		if err := RewriteTFDTFunc(data, spans[half:], func(old int64) int64 {
			return old % (int64(1) << 32)
		}); err != nil {
			return fmt.Errorf("wrap injection (high) low32: %w", err)
		}
		m.Faults = append(m.Faults, Fault{Kind: FaultWrap, Level: "high", Segment: spans[half].Sequence,
			Detail: wrapBase, Note: "second half stored as low 32 bits only; unwrap restores the window"})
		m.Faults = append(m.Faults, Fault{Kind: FaultDrift, Level: "high", Segment: spans[half].Sequence,
			Detail: ppm, Note: "clock runs 800 ppm fast after the midpoint; estimated and corrected"})
		if err := WriteFile(e.Path, data); err != nil {
			return err
		}
	}

	// --- mid: drop one fragment, duplicate another ------------------------
	{
		e := byName["mid"]
		data, spans, err := load(e.Path)
		if err != nil {
			return err
		}
		if len(spans) < 8 {
			return fmt.Errorf("mid representation too short to drop a segment")
		}
		dup := spans[4]
		out := AppendDuplicate(data, dup)
		if err := WriteFile(e.Path, out); err != nil {
			return err
		}
		m.Faults = append(m.Faults, Fault{Kind: FaultDuplicateSegment, Level: "mid", Segment: dup.Sequence,
			Note: "payload and mfhd sequence byte-identical; copy skipped on recovery"})
		data, spans, err = load(e.Path)
		if err != nil {
			return err
		}
		if len(spans) < 8 {
			return fmt.Errorf("mid representation lost segments during duplicate injection")
		}
		drop := spans[5]
		out, err = DropSpans(data, spans, []int{5})
		if err != nil {
			return err
		}
		if err := WriteFile(e.Path, out); err != nil {
			return err
		}
		m.Faults = append(m.Faults, Fault{Kind: FaultDroppedSegment, Level: "mid", Segment: drop.Sequence,
			Note: "mfhd sequence gap; original numbers retained"})
	}

	// --- low: regression, boundary shift, truncated mdat ------------------
	{
		e := byName["low"]
		data, spans, err := load(e.Path)
		if err != nil {
			return err
		}
		if len(spans) < 8 {
			return fmt.Errorf("low representation too short for the mixed programme")
		}
		// Regression: set one mid-file fragment just behind its predecessor so
		// DTS goes backwards. The fragment is isolated by the recovery stage
		// rather than corrupting the cursor for the rest of the run.
		r := (2 * len(spans)) / 5
		// Move only this fragment just behind its predecessor. Later fragments
		// keep their true accumulating tfdt, so the defect is local: recovery
		// isolates this fragment and closes the gap without smearing time over
		// the rest of the representation.
		// Scope to video: audio and video share the fragment but not the
		// tick rate, so a track-blind rewrite would corrupt audio timing.
		videoTS := videoTimescale(data)
		if err := RewriteTFDTTrack(data, spans[r:r+1], 1, func(old int64) int64 {
			prev := track1TFDT(data, spans[r-1])
			if old <= prev {
				return old
			}
			// Sit 1/25 s behind the predecessor's end: one frame of overlap,
			// which must be detected as a regression.
			return prev + int64(videoTS)/25
		}); err != nil {
			return fmt.Errorf("regression injection (low): %w", err)
		}
		m.Faults = append(m.Faults, Fault{Kind: FaultTFDTRegression, Level: "low", Segment: spans[r].Sequence,
			Detail: -100000, Note: "tfdt moves backward; fragment isolated, run continues"})

		// Truncate the final fragment's mdat: declared length exceeds the bytes
		// actually present, so the parser reports a first_bad_byte.
		last := spans[len(spans)-1]
		data, badAt, err := TruncateMdat(data, last)
		if err != nil {
			return err
		}
		if err := WriteFile(e.Path, data); err != nil {
			return err
		}
		m.Faults = append(m.Faults, Fault{Kind: FaultTruncatedMdat, Level: "low", Segment: last.Sequence,
			Detail: badAt, Note: "mdat declared size exceeds available bytes"})

		// Local boundary conflict: shift exactly one mid-file fragment 250 ms
		// late, on each track's own timescale. The representation's first and
		// last boundaries still line up, so the conflict is a per-fragment
		// disagreement the fan-in must report rather than resolve.
		spans, err = loadSpans(e.Path)
		if err != nil {
			return err
		}
		deltas := trackTimescales(data)
		if len(deltas) == 0 {
			deltas = map[uint32]int64{1: 90000, 2: 44100}
		}
		k := len(spans) / 3
		lag := map[uint32]int64{}
		for tid, ts := range deltas {
			lag[tid] = ts / 4 // a quarter of a second on this track
		}
		if err := ShiftTFDTTracks(data, spans[k:k+1], lag); err != nil {
			return fmt.Errorf("boundary injection (low): %w", err)
		}
		if err := WriteFile(e.Path, data); err != nil {
			return err
		}
		m.Faults = append(m.Faults, Fault{Kind: FaultBoundaryConflict, Level: "low", Segment: spans[k].Sequence,
			Detail: 250000, Note: "one fragment shifted 250 ms late; fan-in flags, does not pick"})
	}
	m.Faults = append(m.Faults, Fault{Kind: FaultVFR, Level: "low", Note: "low level uses variable frame durations"})
	return nil
}

// track1TFDT returns track 1's baseDecodeTime in a span.
func track1TFDT(data []byte, sp SegmentSpan) int64 {
	var base int64
	_ = walkMoofChildren(data[sp.MoofOffset:sp.End], func(typ string, rel int) error {
		if typ != "traf" {
			return nil
		}
		traf := data[sp.MoofOffset+int64(rel):]
		if trafTrackID(traf) != 1 {
			return nil
		}
		_ = walkChildren(traf, func(t string, off, size int) error {
			if t != "tfdt" || base != 0 {
				return nil
			}
			payload := off + 8
			if traf[payload] == 1 {
				base = int64(binary.BigEndian.Uint64(traf[payload+4 : payload+12]))
			} else {
				base = int64(binary.BigEndian.Uint32(traf[payload+4 : payload+8]))
			}
			return nil
		})
		return nil
	})
	return base
}

// tfdtOf returns the first traf's baseDecodeTime in a span.
func tfdtOf(data []byte, sp SegmentSpan) int64 {
	var base int64
	_ = walkMoofChildren(data[sp.MoofOffset:sp.End], func(typ string, rel int) error {
		if typ != "traf" {
			return nil
		}
		traf := data[sp.MoofOffset+int64(rel):]
		_ = walkChildren(traf, func(t string, off, size int) error {
			if t != "tfdt" || base != 0 {
				return nil
			}
			payload := sp.MoofOffset + int64(rel+off+8)
			if data[payload] == 1 {
				base = int64(binary.BigEndian.Uint64(data[payload+4 : payload+12]))
			} else {
				base = int64(binary.BigEndian.Uint32(data[payload+4 : payload+8]))
			}
			return nil
		})
		return nil
	})
	return base
}

func load(path string) ([]byte, []SegmentSpan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return data, scanSpans(data), nil
}

func loadSpans(path string) ([]SegmentSpan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return scanSpans(data), nil
}
