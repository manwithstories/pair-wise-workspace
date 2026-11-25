package fixture

import (
	"github.com/fieldcut/fieldcut/internal/base"
	"github.com/fieldcut/fieldcut/internal/ingest"
)

// Injection names accepted by the demo --inject flag.
const (
	InjectTruncAU      = "trunc-au"
	InjectIndexDrift   = "index-drift"
	InjectClockWrap    = "clock-wrap"
	InjectColorConflict = "color-conflict"
)

// ApplyInjections mutates a generated stream to exercise the recovery paths.
// The manifest and bytes stay consistent with the requested fault class so
// the scanner's own detection logic proves the recovery works.
func ApplyInjections(bs *BareStream, injections []string) {
	has := func(name string) bool {
		for _, x := range injections {
			if x == name {
				return true
			}
		}
		return false
	}

	if has(InjectIndexDrift) {
		injectIndexDrift(bs)
	}
	if has(InjectTruncAU) {
		injectTruncatedAU(bs)
	}
	if has(InjectClockWrap) {
		injectClockWrap(bs)
	}
	if has(InjectColorConflict) {
		// Already generated with three conflicting tags; flip the bitstream
		// claim to make the conflict observable at ingest time.
		bs.ColorTags[0].Primaries, bs.ColorTags[0].Transfer = 6, 6
	}
}

// injectIndexDrift shifts one non-keyframe manifest offset forward inside
// the rescan window. The underlying bytes are untouched, so the scanner's
// bounded forward rescan must relocate the real start code.
func injectIndexDrift(bs *BareStream) {
	target := GOP + 1 // an inter frame well past the first keyframe
	if target >= len(bs.Manifest.Entries) {
		return
	}
	e := &bs.Manifest.Entries[target]
	e.Offset += 9
	// Re-derive length so the range check still passes; the scanner
	// corrects the offset and measures the real NAL boundary itself.
	if target+1 < len(bs.Manifest.Entries) {
		next := bs.Manifest.Entries[target+1]
		if int(e.Offset)+e.Length > int(next.Offset) {
			e.Length = int(next.Offset) - int(e.Offset)
		}
	}
}

// injectTruncatedAU simulates a clipped access unit by overwriting the NAL
// header byte with a forbidden/structural-invalid value while leaving the
// start code intact. It targets a manifest-MARKED keyframe (one whose index
// is counted in the keyframe-hit metric), so exactly one indexed keyframe is
// rejected: the 120 indexed keyframes become 119 confirmed hits.
func injectTruncatedAU(bs *BareStream) {
	target := -1
	for i := range bs.Manifest.Entries {
		if bs.Manifest.Entries[i].Keyframe {
			// Pick the third marked keyframe, never the stream's first.
			if markedOrdinal(bs.Manifest, i) == 3 {
				target = i
				break
			}
		}
	}
	if target < 0 {
		return
	}
	e := &bs.Manifest.Entries[target]
	start := int(e.Offset)
	switch bs.Params.Codec {
	case ingest.CodecH264:
		// NAL type 30 is reserved/unspecified and rejected by the scanner's
		// structural validator.
		bs.Data[start+3] = 0x1e
	case ingest.CodecH265:
		// nal_unit_type occupies the high six bits; type 41 (0x52 shift) is
		// reserved and rejected by the structural validator.
		bs.Data[start+3] = 0x52
	case ingest.CodecAV1:
		// OBU forbidden bit (high bit) must be zero.
		bs.Data[start+3] = 0x80
	}
}

// markedOrdinal returns the 1-based position of entry i among marked
// keyframes, or 0 if the entry is not marked.
func markedOrdinal(m *ingest.Manifest, i int) int {
	n := 0
	for j := 0; j <= i; j++ {
		if m.Entries[j].Keyframe {
			n++
		}
	}
	return n
}

// injectClockWrap rewrites native PTS values to simulate a real 33-bit RTP
// timestamp wrap: the pre-mark raw values climb toward the modulus, then the
// post-mark values roll under to small numbers and climb again. The shared
// clock must add the modulus back and keep the output PTS strictly
// increasing; one small backwards step also exercises the jitter path.
func injectClockWrap(bs *BareStream) {
	if len(bs.Manifest.Entries) < GOP*4 {
		return
	}
	mark := len(bs.Manifest.Entries) * 2 / 3
	period := base.WrapPeriod
	step := bs.Manifest.TimeBase / FrameRate
	// Keep every raw value inside the 33-bit field. Before the mark the
	// counter sits at modulus-N*step and rises; after it rolls to a small
	// value and rises again.
	for i := 0; i < mark; i++ {
		bs.Manifest.Entries[i].PTS = period - uint64(mark-i)*step
	}
	for i := mark; i < len(bs.Manifest.Entries); i++ {
		bs.Manifest.Entries[i].PTS = uint64(i-mark) * step
	}
	// One small genuine backwards step (half a frame) exercises restart
	// handling without leaving the 33-bit field.
	if mark+1 < len(bs.Manifest.Entries) {
		bs.Manifest.Entries[mark+1].PTS = bs.Manifest.Entries[mark].PTS - step/2
	}
}

// ManifestJSON re-serializes the (possibly injected) manifest for the demo.
func ManifestJSON(m *ingest.Manifest) ([]byte, error) { return m.MarshalJSON() }

// ManifestTSV renders the manifest as the eight-column TSV format.
func ManifestTSV(m *ingest.Manifest) []byte {
	var buf []byte
	for _, e := range m.Entries {
		key := "0"
		if e.Keyframe {
			key = "1"
		}
		buf = append(buf, joinTSV(e.Camera, e.Rendition, string(e.Codec),
			utoa(e.TimeBase), utoa(e.PTS), itoa64(e.Offset), itoa(e.Length), key)...)
		buf = append(buf, '\n')
	}
	return buf
}

func joinTSV(fields ...string) []byte {
	var out []byte
	for i, f := range fields {
		if i > 0 {
			out = append(out, '\t')
		}
		out = append(out, f...)
	}
	return out
}

func utoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

func itoa(v int) string { return utoa(uint64(v)) }

func itoa64(v int64) string {
	if v < 0 {
		return "-" + utoa(uint64(-v))
	}
	return utoa(uint64(v))
}
