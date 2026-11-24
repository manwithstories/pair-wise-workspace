package fixture

import (
	"os"
	"path/filepath"
	"strings"

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
		// Duplicates are the headline defect, but alone they are recoverable
		// warnings; combine one segment's duplication with a burst loss on the
		// second segment so the case also yields a damaged range.
		return injectCapture(t, func(frames []frameEnvelope) []frameEnvelope {
			return burstLossMutator(duplicateMutator(frames))
		})
	case CaseReorder:
		// Reordering is the headline behavior and stays inside the window
		// (fully recoverable, reported as warnings); a second fault segment also
		// carries a window-overflow burst so the case yields damaged ranges.
		return injectCapture(t, func(frames []frameEnvelope) []frameEnvelope {
			return burstLossOnOneSegment(reorderMutator(frames), faultSegB)
		})
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

// reorderMutator swaps two adjacent packets in the middle of each fault
// segment while keeping every packet. The swap stays inside the reorder
// window, so recovery fully restores the byte stream (no damage) but records
// reordered events that propagate into the analysis timeline.
func reorderMutator(frames []frameEnvelope) []frameEnvelope {
	out := append([]frameEnvelope(nil), frames...)
	swapMiddlePair(out, faultSegA)
	swapMiddlePair(out, faultSegB)
	return out
}

func swapMiddlePair(frames []frameEnvelope, segment int) {
	var idxs []int
	for i, f := range frames {
		if f.fragment == segment {
			idxs = append(idxs, i)
		}
	}
	if len(idxs) < 4 {
		return
	}
	mid := len(idxs) / 2
	a, b := idxs[mid], idxs[mid+1]
	frames[a], frames[b] = frames[b], frames[a]
}

// burstLossOnOneSegment applies the window-overflow middle-burst drop to a
// single fragment, leaving the other fault segment to demonstrate recoverable
// reordering.
func burstLossOnOneSegment(frames []frameEnvelope, onlySegment int) []frameEnvelope {
	const headKeep = 4
	dropRun := transport.DefaultReorderWindow + 3
	seen := map[int]int{}
	var out []frameEnvelope
	for _, f := range frames {
		idx := seen[f.fragment]
		seen[f.fragment] = idx + 1
		if f.fragment == onlySegment && idx >= headKeep && idx < headKeep+dropRun {
			continue
		}
		out = append(out, f)
	}
	return out
}

// burstLossMutator keeps the head packets of each fault fragment, removes a
// middle run longer than the reorder window, and keeps the tail. Because the
// reconstructor has already established the fragment frontier from the head,
// the surviving tail arrives more than one window ahead of the expected next
// sequence, forcing a declared (never silently filled) burst-loss gap.
func burstLossMutator(frames []frameEnvelope) []frameEnvelope {
	const headKeep = 4
	dropRun := transport.DefaultReorderWindow + 3
	seen := map[int]int{}
	var out []frameEnvelope
	for _, f := range frames {
		idx := seen[f.fragment]
		seen[f.fragment] = idx + 1
		if f.fragment == faultSegA || f.fragment == faultSegB {
			if idx >= headKeep && idx < headKeep+dropRun {
				continue // dropped middle burst
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

// injectClockReset rewrites both the DASH MPD and HLS media playlists so a
// segment declares a backwards media time. The manifest parser must reject
// the non-monotonic timeline instead of treating the clock reset as valid.
func injectClockReset(t *Tree) error {
	if err := injectDASHClockReset(t); err != nil {
		return err
	}
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

// injectDASHClockReset turns the fixed-duration templates into explicit
// SegmentTimeline lists containing a backwards <S> entry, which the DASH
// parser rejects as a timestamp regression.
func injectDASHClockReset(t *Tree) error {
	data, err := os.ReadFile(t.DASHMPD)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "<SegmentTemplate") {
			b.WriteString(line)
			b.WriteString("\n")
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
		// Extract the per-rep attributes we must preserve.
		initSeg := attr(line, "initialization")
		mediaPat := attr(line, "media")
		b.WriteString(indent + `<SegmentTemplate timescale="1000" initialization="` +
			initSeg + `" media="` + mediaPat + `">` + "\n")
		b.WriteString(indent + "  <SegmentTimeline>\n")
		b.WriteString(indent + `    <S t="0" d="1000"/>` + "\n")
		// Illegal regression: 500 < previous end (1000).
		b.WriteString(indent + `    <S t="500" d="1000"/>` + "\n")
		b.WriteString(indent + "  </SegmentTimeline>\n")
		b.WriteString(indent + "</SegmentTemplate>\n")
	}
	return os.WriteFile(t.DASHMPD, []byte(strings.TrimRight(b.String(), "\n")+"\n"), 0o644)
}

// attr extracts an XML attribute value from a single element line.
func attr(line, name string) string {
	key := name + "=\""
	i := strings.Index(line, key)
	if i < 0 {
		return ""
	}
	rest := line[i+len(key):]
	j := strings.Index(rest, "\"")
	if j < 0 {
		return ""
	}
	return rest[:j]
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
