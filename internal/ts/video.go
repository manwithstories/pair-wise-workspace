package ts

// NAL unit types for H.264 (ITU-T H.264 Table 7-1), limited to those cinepipe
// inspects when locating access units and random-access points.
const (
	NALSlice      = 1
	NALSliceDPA   = 2
	NALSliceDPB   = 3
	NALSliceDPC   = 4
	NALSliceIDR   = 5
	NALSEI        = 6
	NALSPS        = 7
	NALPPS        = 8
	NALAUD        = 9
	NALEndSeq     = 10
	NALEndStream  = 11
	NALFillerData = 12
)

// HEVC NAL types cinepipe recognises for random-access detection.
const (
	HEVCCRA = 21
	HEVCIDR = 19
	HEVCBLA = 20
)

// AccessUnit is one coded video frame as assembled from PES payloads.
// ByteOffset points at the first TS packet that carries the unit so the report
// can map a keyframe back to the input.
type AccessUnit struct {
	Index      int64
	ByteOffset int64
	Ticks      int64 // absolute 90 kHz PTS, 0 when absent
	Duration   int64 // ticks to the next unit, 0 if unknown
	Keyframe   bool
	Size       int
	SliceCount int
	// SampleData is the VCL payload (slice NAL bodies, start codes included)
	// captured for fMP4 muxing. It is nil unless CaptureSamples was enabled.
	SampleData []byte
	// SPS and PPS hold the parameter-set bodies seen in the unit, in raw
	// byte form, when CaptureSamples is enabled. The muxer uses the first pair
	// to build avcC.
	SPS []byte
	PPS []byte
}

// VideoAssembler builds access units from a video PID's elementary stream. It
// tolerates PES boundaries and truncated packets: a unit cut short keeps the
// NALs it received, and the caller learns the unit was damaged from the fault
// stream rather than from a rewriting of the index.
//
// Boundaries are detected in two ways because real streams vary: an AUD always
// opens a new unit, and a PES carrying a distinct PTS closes the current unit
// once it already holds slice data. SPS/PPS that follow slice data also close a
// unit for streams that omit AUDs.
type VideoAssembler struct {
	streamType uint8
	buf        []byte
	nextIndex  int64

	// Per-PES context, refreshed on every PushPES.
	curOff  int64
	curPESPTS int64

	// In-flight unit state.
	inUnit   bool
	startOff int64
	curPTS   int64
	curKey   bool
	curVCL   bool
	curSize  int
	curSlices int
	samples   bool
	curData   []byte
	curSPS    []byte
	curPPS    []byte

	units []AccessUnit
}

// NewVideoAssembler returns an assembler for the given stream type.
func NewVideoAssembler(streamType uint8) *VideoAssembler {
	return &VideoAssembler{streamType: streamType}
}

// SetCaptureSamples enables retention of the per-frame sample bytes and the
// SPS/PPS bodies. The scanner leaves it off for pure indexing; the deliverer
// turns it on so it can build real fMP4 samples.
func (v *VideoAssembler) SetCaptureSamples(on bool) { v.samples = on }

// PushPES ingests a PES payload that carries elementary video data. off is the
// byte offset of the TS packet that began the PES; pts is the absolute PTS,
// 0 when absent.
func (v *VideoAssembler) PushPES(payload []byte, off, pts int64) {
	if len(payload) == 0 {
		return
	}
	v.curOff = off
	v.curPESPTS = pts

	// A new PES timestamp means a new access unit begins. Finalize any NAL
	// still buffered so keyframe detection for the closing unit is complete
	// before the boundary check.
	boundary := v.inUnit && pts != 0 && v.curPTS != 0 && pts != v.curPTS
	if boundary {
		v.finalizePending()
	}
	if boundary && v.curVCL {
		v.emitUnit()
		v.beginUnit(off, pts)
	} else if !v.inUnit {
		v.beginUnit(off, pts)
	}
	if v.curPTS == 0 && pts != 0 {
		v.curPTS = pts
	}
	v.buf = append(v.buf, payload...)
	v.scan()
}

// finalizePending treats the buffered bytes as the trailing NAL of the current
// unit. It is called when a unit boundary is known (a PES timing change) or at
// end of stream; a NAL split across PES payloads with an unchanged timestamp is
// still reassembled by scan before that.
func (v *VideoAssembler) finalizePending() {
	if len(v.buf) == 0 {
		return
	}
	start := findStartCode(v.buf, 0)
	if start < 0 {
		v.buf = nil
		return
	}
	body := start + startCodeLen(v.buf[start:])
	if body < len(v.buf) {
		v.handleNAL(v.buf[body:])
	}
	v.buf = nil
}

// beginUnit starts recording a fresh access unit.
func (v *VideoAssembler) beginUnit(off, pts int64) {
	v.inUnit = true
	v.startOff = off
	v.curPTS = pts
	v.curKey = false
	v.curVCL = false
	v.curSize = 0
	v.curSlices = 0
}

// scan consumes complete NAL units, leaving a trailing partial NAL buffered
// until the next push completes it.
func (v *VideoAssembler) scan() {
	for {
		start := findStartCode(v.buf, 0)
		if start < 0 {
			if len(v.buf) > 3 {
				v.buf = v.buf[len(v.buf)-3:]
			}
			return
		}
		body := start + startCodeLen(v.buf[start:])
		next := findStartCode(v.buf, body)
		if next < 0 {
			if start > 0 {
				v.buf = v.buf[start:]
			}
			return
		}
		v.handleNAL(v.buf[body:next])
		v.buf = v.buf[next:]
	}
}

// nalTypeByte decodes the NAL unit type for this stream's codec.
func (v *VideoAssembler) nalTypeByte(nal []byte) uint8 {
	if v.streamType == StreamTypeHEVC {
		return (nal[0] >> 1) & 0x3F
	}
	return nal[0] & 0x1F
}

// handleNAL classifies one complete NAL unit and updates unit state. NALs seen
// before any PES opened a unit still open one at offset zero.
func (v *VideoAssembler) handleNAL(nal []byte) {
	if len(nal) == 0 {
		return
	}
	if !v.inUnit {
		v.beginUnit(v.curOff, v.curPESPTS)
	}
	t := v.nalTypeByte(nal)

	if v.streamType == StreamTypeHEVC {
		switch {
		case t == 32 || t == 33 || t == 34: // VPS, SPS, PPS
			if v.curVCL {
				v.closeAndReopen()
			}
			v.curSize += len(nal)
		case t == 35: // AUD
			v.rotateForHeader()
			v.curSize += len(nal)
		case t == HEVCIDR || t == HEVCCRA || t == HEVCBLA:
			v.curVCL = true
			v.curKey = true
			v.curSlices++
			v.curSize += len(nal)
		case t <= 9:
			v.curVCL = true
			v.curSlices++
			v.curSize += len(nal)
		default:
			v.curSize += len(nal)
		}
		return
	}

	switch t {
	case NALAUD:
		v.rotateForHeader()
		v.curSize += len(nal)
	case NALSPS, NALPPS:
		if v.samples {
			if t == NALSPS {
				v.curSPS = append([]byte(nil), nal...)
			} else {
				v.curPPS = append([]byte(nil), nal...)
			}
		}
		if v.curVCL {
			v.closeAndReopen()
		}
		v.curSize += len(nal)
	case NALSlice, NALSliceDPA, NALSliceDPB, NALSliceDPC:
		v.curVCL = true
		v.curSlices++
		v.curSize += len(nal)
		v.collectVCL(nal)
	case NALSliceIDR:
		v.curVCL = true
		v.curKey = true
		v.curSlices++
		v.curSize += len(nal)
		v.collectVCL(nal)
	default:
		v.curSize += len(nal)
	}
}

// collectVCL stores a slice NAL's bytes (including the start-code prefix) when
// sample capture is enabled, so the muxer can emit it verbatim in mdat.
func (v *VideoAssembler) collectVCL(nal []byte) {
	if !v.samples {
		return
	}
	v.curData = append(v.curData, 0x00, 0x00, 0x00, 0x01)
	v.curData = append(v.curData, nal...)
}

// rotateForHeader begins a new unit for a parameter-set or AUD boundary. A
// unit that never accumulated slices is folded into the new unit rather than
// emitted, so header-only runs do not inflate the frame count. The header NAL
// that triggered the rotation belongs to the unit it opens.
func (v *VideoAssembler) rotateForHeader() {
	if v.curSlices == 0 {
		return // same unit continues; just append the header
	}
	v.emitUnit()
	v.beginUnit(v.curOff, v.curPESPTS)
}

// closeAndReopen closes the in-flight unit and begins a fresh one whose NAL
// arrives right after the boundary marker.
func (v *VideoAssembler) closeAndReopen() {
	v.emitUnit()
	v.beginUnit(v.curOff, v.curPESPTS)
}

// closeUnit resets the in-flight state; emitUnit appends it only if it holds
// pictures.
func (v *VideoAssembler) closeUnit() {
	if !v.inUnit {
		return
	}
	if v.curSlices > 0 {
		v.emitUnit()
	}
	v.inUnit = false
}

// emitUnit appends the in-flight access unit and resets unit state. When
// sample capture is on, the collected slices and parameter sets travel with the
// unit; otherwise those fields stay nil.
func (v *VideoAssembler) emitUnit() {
	if !v.inUnit || v.curSlices == 0 {
		return
	}
	u := AccessUnit{
		Index:      v.nextIndex,
		ByteOffset: v.startOff,
		Ticks:      v.curPTS,
		Keyframe:   v.curKey,
		Size:       v.curSize,
		SliceCount: v.curSlices,
	}
	if v.samples {
		u.SampleData = v.curData
		u.SPS = v.curSPS
		u.PPS = v.curPPS
	}
	v.units = append(v.units, u)
	v.nextIndex++
	v.curKey = false
	v.curVCL = false
	v.curSize = 0
	v.curSlices = 0
	v.curData = nil
	v.curSPS = nil
	v.curPPS = nil
}

// Flush closes any in-flight unit at end of stream and derives durations from
// successive timestamps. The trailing buffered NAL has no following start code
// to delimit it, so it is completed here.
func (v *VideoAssembler) Flush() {
	v.finalizePending()
	v.closeUnit()
	for i := 0; i+1 < len(v.units); i++ {
		if v.units[i].Ticks != 0 && v.units[i+1].Ticks != 0 {
			v.units[i].Duration = v.units[i+1].Ticks - v.units[i].Ticks
		}
	}
}

// Units returns the assembled access units.
func (v *VideoAssembler) Units() []AccessUnit { return v.units }

// KeyframeIntervals returns the deltas between successive keyframe timestamps,
// ignoring gaps where a timestamp is unknown.
func KeyframeIntervals(units []AccessUnit) []int64 {
	var keys []int64
	for _, u := range units {
		if u.Keyframe && u.Ticks != 0 {
			keys = append(keys, u.Ticks)
		}
	}
	var out []int64
	for i := 1; i < len(keys); i++ {
		out = append(out, keys[i]-keys[i-1])
	}
	return out
}

// findStartCode returns the index of the next 00 00 01 sequence at or after
// from, or -1.
func findStartCode(b []byte, from int) int {
	for i := from; i+3 <= len(b); i++ {
		if b[i] == 0x00 && b[i+1] == 0x00 && b[i+2] == 0x01 {
			return i
		}
	}
	return -1
}

// startCodeLen reports the total length of the start code at the head of b,
// counting a leading zero so a 00 00 00 01 prefix measures four bytes. b must
// begin a start code.
func startCodeLen(b []byte) int {
	zeros := 0
	for zeros < len(b) && b[zeros] == 0x00 {
		zeros++
	}
	if zeros >= 3 {
		return zeros + 1 // includes the 0x01
	}
	return 3
}
