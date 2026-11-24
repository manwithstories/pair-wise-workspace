package fixture

import (
	"os"
	"path/filepath"

	"mediac/internal/transport"
)

// Fault segment indexes (1-based, shared across reps for a simple story).
const (
	faultSegA = 7  // primary damaged segment
	faultSegB = 15 // second damaged segment (>=2 damaged ranges required)
)

// buildFault starts from the clean material, then mutates the RTP capture or
// fragment bytes so the corresponding defect is real and detectable end to
// end. Transport faults (duplicate/reorder/burst-loss) propagate through
// recovery into analysis and scheduling; truncated-box is injected at the
// bitstream layer; clock-reset rewinds the HLS/DASH timeline.
func buildFault(t *Tree, caseName string) error {
	if err := buildClean(t); err != nil {
		return err
	}
	switch caseName {
	case CaseDuplicate:
		return injectCapture(t, duplicateMutator)
	case CaseReorder:
		return injectCapture(t, reorderMutator)
	case CaseBurstLoss:
		return injectCapture(t, burstLossMutator)
	case CaseTruncatedBox:
		return injectTruncatedBox(t)
	case CaseClockReset:
		return injectClockReset(t)
	default:
		return errUnknownCase(caseName)
	}
}

// captureFrames reads the clean capture back into frame envelopes so a
// mutator can reorder, duplicate or drop individual RTP packets.
type frameEnvelope struct {
	repID    string
	fragment int
	raw      []byte // complete .rtpr frame
}

func readFrames(path string) ([]frameEnvelope, error) {
	all, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []frameEnvelope
	pos := 0
	for pos < len(all) {
		if pos+12 > len(all) {
			break
		}
		repLen := int(all[pos+5])
		fragment := int(uint16(all[pos+6])<<8 | uint16(all[pos+7]))
		repID := string(all[pos+12 : pos+12+repLen])
		lenOff := pos + 12 + repLen
		pktLen := int(uint16(all[lenOff])<<8 | uint16(all[lenOff+1]))
		end := lenOff + 2 + pktLen
		out = append(out, frameEnvelope{
			repID: repID, fragment: fragment,
			raw: append([]byte(nil), all[pos:end]...),
		})
		pos = end
	}
	return out, nil
}

func writeFrames(path string, frames []frameEnvelope) error {
	var all []byte
	for _, f := range frames {
		all = append(all, f.raw...)
	}
	return os.WriteFile(path, all, 0o644)
}

// injectCapture rebuilds the capture with a per-representation mutator applied
// to the frames belonging to the two fault segments.
func injectCapture(t *Tree, mutate func(frames []frameEnvelope) []frameEnvelope) error {
	frames, err := readFrames(t.RTPRecord)
	if err != nil {
		return err
	}
	byRep := map[string][]frameEnvelope{}
	var order []string
	for _, f := range frames {
		if _, ok := byRep[f.repID]; !ok {
			order = append(order, f.repID)
		}
		byRep[f.repID] = append(byRep[f.repID], f)
	}
	var rebuilt []frameEnvelope
	for _, rep := range order {
		rebuilt = append(rebuilt, mutate(byRep[rep])...)
	}
	return writeFrames(t.RTPRecord, rebuilt)
}

// duplicateMutator repeats every packet of faultSegA, and one packet of
// faultSegB, inside the same representation.
func duplicateMutator(frames []frameEnvelope) []frameEnvelope {
	var out []frameEnvelope
	dupB := false
	for _, f := range frames {
		out = append(out, f)
		switch f.fragment {
		case faultSegA:
			out = append(out, f) // whole-segment duplication
		case faultSegB:
			if !dupB {
				out = append(out, f) // single duplicate packet
				dupB = true
			}
		}
	}
	return out
}

// reorderMutator swaps adjacent packets within the fault segments while
// keeping every packet, so the bounded window recovers the byte stream but
// records reordered events.
func reorderMutator(frames []frameEnvelope) []frameEnvelope {
	out := append([]frameEnvelope(nil), frames...)
	swapWithinSegment(out, faultSegA)
	swapWithinSegment(out, faultSegB)
	return out
}

func swapWithinSegment(frames []frameEnvelope, segment int) {
	var idxs []int
	for i, f := range frames {
		if f.fragment == segment {
			idxs = append(idxs, i)
		}
	}
	if len(idxs) >= 2 {
		frames[idxs[0]], frames[idxs[1]] = frames[idxs[1]], frames[idxs[0]]
	}
}

// burstLossMutator removes a contiguous run of packets from each fault
// segment. The removed span exceeds the default reorder window once later
// packets arrive, so recovery reports unrecovered burst gaps rather than
// silently filling them.
func burstLossMutator(frames []frameEnvelope) []frameEnvelope {
	drop := map[int]int{faultSegA: 0, faultSegB: 0}
	var out []frameEnvelope
	for _, f := range frames {
		if n, ok := drop[f.fragment]; ok {
			// Drop the first packets of each affected fragment (a burst at the
			// fragment head), keeping the later packets that force the window
			// overflow.
			dropCount := transport.DefaultReorderWindow + 2
			if n < dropCount {
				drop[f.fragment] = n + 1
				continue
			}
		}
		out = append(out, f)
	}
	return out
}

// injectTruncatedBox cuts the mdat payload short on disk for both fault
// segments while leaving the declared box size intact.
func injectTruncatedBox(t *Tree) error {
	for _, spec := range Layout {
		for _, seg := range []int{faultSegA, faultSegB} {
			path := filepath.Join(t.FragmentDir[spec.ID], segFileName(seg))
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			cut := len(data) * 3 / 4
			if err := os.WriteFile(path, data[:cut], 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// injectClockReset rewrites the HLS media playlists and DASH templates so the
// second fault segment declares a backwards media time. The manifest parser
// must reject the non-monotonic timeline instead of treating it as valid.
func injectClockReset(t *Tree) error {
	for _, spec := range Layout {
		media := t.HLSMedia[spec.ID]
		data, err := os.ReadFile(media)
		if err != nil {
			return err
		}
		text := string(data)
		// Insert a negative EXTINF at the fault segment boundary: the parser
		// classifies negative durations as an illegal clock rewind.
		needle := "#EXTINF:1.000,\n" + segFileName(faultSegA)
		replacement := "#EXTINF:-1.000,\n" + segFileName(faultSegA)
		rewound := replaceFirst(text, needle, replacement)
		if err := os.WriteFile(media, []byte(rewound), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func segFileName(seg int) string {
	return "seg-" + itoa(seg) + ".m4s"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func replaceFirst(s, old, new string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + new + s[i+len(old):]
		}
	}
	return s
}
