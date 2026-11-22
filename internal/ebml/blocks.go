package ebml

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"

	"archivecheck/internal/clock"
	"archivecheck/internal/fault"
)

// TrackKind classifies a Matroska track type.
type TrackKind uint8

const (
	TrackVideo    TrackKind = 1
	TrackAudio    TrackKind = 2
	TrackSubtitle TrackKind = 17
)

func (k TrackKind) String() string {
	switch k {
	case TrackVideo:
		return "video"
	case TrackAudio:
		return "audio"
	case TrackSubtitle:
		return "subtitle"
	}
	return fmt.Sprintf("track-type-%d", uint8(k))
}

// Track is the subset of TrackEntry needed to convert block timestamps.
type Track struct {
	Number         uint64
	UID            uint64
	Kind           TrackKind
	CodecID        string
	DefaultDur     uint64         // nanoseconds per frame, 0 when absent
	TimestampScale clock.Rational // track-local multiplier, default 1
	Language       string
	PixelWidth     uint64
	PixelHeight    uint64
	Channels       uint64
	SampleRate     uint64
}

// Header is the parsed document metadata that blocks depend on.
type Header struct {
	TimestampScale int64 // segment ticks per second, default 1_000_000
	DurationTicks  float64
	Tracks         map[uint64]*Track
	TrackOrder     []uint64
}

// Time converts a segment tick count to exact media seconds, applying both
// the segment TimestampScale and the per-track TrackTimestampScale.
func (h *Header) Time(track uint64, ticks int64) (clock.Rational, error) {
	scale := int64(1_000_000)
	if h.TimestampScale > 0 {
		scale = h.TimestampScale
	}
	t, err := clock.FromTicks(ticks, scale)
	if err != nil {
		return clock.Rational{}, err
	}
	if tr, ok := h.Tracks[track]; ok && !tr.TimestampScale.IsZero() {
		t = t.Mul(tr.TimestampScale)
	}
	return t, nil
}

// Lace describes block lacing.
type Lace struct {
	Kind   string
	Frames int
	Sizes  []int64
}

// Block is one reassembled track block with source provenance.
type Block struct {
	Track       uint64
	Element     ID
	Offset      int64 // header offset of the SimpleBlock/Block element
	PayloadOff  int64
	ClusterTick int64
	RelTick     int16
	Keyframe    bool
	Invisible   bool
	Discardable bool
	Duration    uint64
	Lace        Lace
	// FrameBytes is the total payload after the block header.
	FrameBytes int64
	// FrameDigest is a CRC over the frame payload, computed while streaming.
	FrameDigest uint32
	Time        clock.Rational
}

// BlockReader reassembles blocks from a reader walk.
type BlockReader struct {
	rd     *Reader
	faults *fault.Set

	header Header

	inCluster   bool
	clusterTick int64

	// Regions whose children are parsed out-of-band.
	skipDepth int
	skipUntil int64

	onBlock func(*Block) error
	blocks  []*Block
}

// NewBlockReader wires a block reader over rd.
func NewBlockReader(rd *Reader) *BlockReader {
	return &BlockReader{rd: rd, faults: rd.Faults()}
}

// NewBlockReaderWithFaults wires a block reader with an explicit fault sink,
// which may differ from the reader's (or be the same set).
func NewBlockReaderWithFaults(rd *Reader, fs *fault.Set) *BlockReader {
	return &BlockReader{rd: rd, faults: fs}
}

// Blocks returns the blocks accumulated so far, in document order.
func (b *BlockReader) Blocks() []*Block { return b.blocks }

// Header returns metadata discovered so far.
func (b *BlockReader) Header() Header { return b.header }

// OnBlock installs a callback invoked in document order; returning an error
// stops the walk.
func (b *BlockReader) OnBlock(fn func(*Block) error) { b.onBlock = fn }

// Scan walks the whole document, parsing metadata and blocks.
func (b *BlockReader) Scan() error {
	b.header.TimestampScale = 1_000_000
	b.header.Tracks = make(map[uint64]*Track)
	return b.rd.Walk(b.visit)
}

func (b *BlockReader) report(k fault.Kind, off int64, msg string) {
	if b.faults == nil {
		return
	}
	b.faults.Add(fault.New(fault.ModuleEBML, k, fault.NewOffset(uint64(off)), msg))
}

func (b *BlockReader) visit(ph Phase, el Element) error {
	// Ignore events from regions already parsed directly.
	if b.skipDepth > 0 {
		if ph == PhaseExit && el.HeaderOffset == b.skipUntil && el.Depth == b.skipDepth-1 {
			b.skipDepth = 0
		}
		if el.Depth >= b.skipDepth {
			return nil
		}
	}
	switch ph {
	case PhaseEnter:
		switch el.ID {
		case IDCluster:
			b.inCluster = true
			b.clusterTick = 0
		case IDTrackEntry:
			b.readTrackEntry(el)
			b.beginSkip(el)
		case IDBlockGroup:
			b.readBlockGroup(el)
			b.beginSkip(el)
		}
		return nil
	case PhaseExit:
		if el.ID == IDCluster {
			b.inCluster = false
		}
		return nil
	}
	if el.Truncated {
		return nil // reported by the reader; never emit a partial block
	}
	switch el.ID {
	case IDTimestampScale:
		if v, ok := readUint(payloadOf(b.rd, el, 8)); ok && v > 0 {
			b.header.TimestampScale = int64(v)
		}
	case IDDuration:
		if v, ok := readFloat(payloadOf(b.rd, el, 8)); ok {
			b.header.DurationTicks = v
		}
	case IDClusterTimestamp:
		if v, ok := readUint(payloadOf(b.rd, el, 8)); ok {
			b.clusterTick = int64(v)
		}
	case IDSimpleBlock:
		return b.parseBlock(el, IDSimpleBlock, 0, true)
	}
	return nil
}

// beginSkip marks el's subtree as handled; the reader still walks it, so the
// visitor drops those events until leaving the element.
func (b *BlockReader) beginSkip(el Element) {
	b.skipDepth = el.Depth + 1
	b.skipUntil = el.HeaderOffset
}

// subScan walks the direct children of el, invoking fn for leaf elements.
func (b *BlockReader) subScan(el Element, fn func(Element) error) {
	end := el.PayloadEnd
	if end < 0 {
		end = b.rd.Size()
	}
	_, _ = b.rd.scan(el.PayloadOffset, end, el.Level, el.Depth+1, el.ID, func(ph Phase, child Element) error {
		if ph == PhaseLeaf {
			return fn(child)
		}
		return nil
	}, false)
}

func (b *BlockReader) readTrackEntry(el Element) {
	tr := &Track{TimestampScale: clock.MustNew(1, 1)}
	var codecPrivate []byte
	b.subScan(el, func(c Element) error {
		if c.Truncated {
			return nil
		}
		switch c.ID {
		case IDTrackNumber:
			if v, ok := readUint(payloadOf(b.rd, c, 8)); ok {
				tr.Number = v
			}
		case IDTrackUID:
			if v, ok := readUint(payloadOf(b.rd, c, 8)); ok {
				tr.UID = v
			}
		case IDTrackType:
			if v, ok := readUint(payloadOf(b.rd, c, 8)); ok {
				tr.Kind = TrackKind(v)
			}
		case IDCodecID:
			raw := payloadOf(b.rd, c, 64)
			tr.CodecID = string(raw)
		case IDCodecPrivate:
			codecPrivate = payloadOf(b.rd, c, 256)
		case IDDefaultDuration:
			if v, ok := readUint(payloadOf(b.rd, c, 8)); ok {
				tr.DefaultDur = v
			}
		case IDTrackTimestampScale:
			if v, ok := readFloat(payloadOf(b.rd, c, 8)); ok && v > 0 {
				scale, err := clock.ParseDecimal(fmt.Sprintf("%g", v))
				if err == nil {
					tr.TimestampScale = scale
				}
			}
		case IDLanguage:
			tr.Language = string(payloadOf(b.rd, c, 32))
		case IDPixelWidth:
			if v, ok := readUint(payloadOf(b.rd, c, 8)); ok {
				tr.PixelWidth = v
			}
		case IDPixelHeight:
			if v, ok := readUint(payloadOf(b.rd, c, 8)); ok {
				tr.PixelHeight = v
			}
		case IDChannels:
			if v, ok := readUint(payloadOf(b.rd, c, 8)); ok {
				tr.Channels = v
			}
		case IDSamplingFrequency:
			if v, ok := readFloat(payloadOf(b.rd, c, 8)); ok {
				tr.SampleRate = uint64(v)
			}
		}
		return nil
	})
	_ = codecPrivate
	if tr.Number == 0 {
		b.report(fault.KindBadElement, el.HeaderOffset, "TrackEntry without TrackNumber")
		return
	}
	if prev, ok := b.header.Tracks[tr.Number]; ok {
		f := fault.New(fault.ModuleEBML, fault.KindBadElement, fault.NewOffset(uint64(el.HeaderOffset)),
			fmt.Sprintf("duplicate TrackNumber %d (previous UID %d)", tr.Number, prev.UID))
		f.OnTrack(tr.Number)
		if b.faults != nil {
			b.faults.Add(f)
		}
		return
	}
	b.header.Tracks[tr.Number] = tr
	b.header.TrackOrder = append(b.header.TrackOrder, tr.Number)
}

func (b *BlockReader) readBlockGroup(el Element) {
	var dur uint64
	var hasRef bool
	var blockEl *Element
	b.subScan(el, func(c Element) error {
		if c.Truncated {
			return nil
		}
		switch c.ID {
		case IDBlockDuration:
			if v, ok := readUint(payloadOf(b.rd, c, 8)); ok {
				dur = v
			}
		case IDReferenceBlock:
			hasRef = true
		case IDBlock:
			cp := c
			blockEl = &cp
		}
		return nil
	})
	if blockEl == nil {
		b.report(fault.KindBadElement, el.HeaderOffset, "BlockGroup without Block")
		return
	}
	_ = b.parseBlock(*blockEl, IDBlock, dur, !hasRef)
}

func (b *BlockReader) parseBlock(el Element, kind ID, duration uint64, simpleKey bool) error {
	if !b.inCluster {
		b.report(fault.KindBadElement, el.HeaderOffset, "block outside a cluster")
		return nil
	}
	payloadLen := el.PayloadLen()
	if payloadLen < 4 {
		b.report(fault.KindTruncated, el.HeaderOffset, "block payload shorter than its 4-byte header")
		return nil
	}
	// Header: track vint (<=8) + 2-byte relative timestamp + 1 flags + lacing.
	hdr, err := b.rd.ReadAt(el.PayloadOffset, min64(payloadLen, 4+8+256))
	if err != nil {
		return err
	}
	if len(hdr) < 4 {
		b.report(fault.KindTruncated, el.HeaderOffset, "block header truncated")
		return nil
	}
	track, _, sizeLen, err := ParseTrackNumber(hdr)
	if err != nil {
		b.report(fault.KindBadElement, el.HeaderOffset, err.Error())
		return nil
	}
	rel := int16(binary.BigEndian.Uint16(hdr[sizeLen:]))
	flags := hdr[2+sizeLen]
	blk := &Block{
		Track:       track,
		Element:     kind,
		Offset:      el.HeaderOffset,
		PayloadOff:  el.PayloadOffset,
		ClusterTick: b.clusterTick,
		RelTick:     rel,
		Keyframe:    kind == IDSimpleBlock && simpleKey && flags&0x80 != 0,
		Invisible:   flags&0x08 != 0,
		Discardable: flags&0x01 != 0,
		Duration:    duration,
	}
	if kind == IDBlock {
		blk.Keyframe = simpleKey
	}
	headerLen := int64(3 + sizeLen)
	body := hdr[headerLen:]
	laceBits := (flags & 0x06) >> 1
	var descLen int64
	switch laceBits {
	case 0:
		blk.Lace = Lace{Kind: "none", Frames: 1}
	case 1, 3:
		n, sizes, used, perr := parseXLace(body, laceBits == 3)
		if perr != nil {
			b.report(fault.KindBadElement, el.HeaderOffset, perr.Error())
			return nil
		}
		blk.Lace = Lace{Kind: laceName(laceBits), Frames: n, Sizes: sizes}
		descLen = int64(used)
	case 2:
		n, sizes, used, perr := parseFixedLace(body, payloadLen-headerLen)
		if perr != nil {
			b.report(fault.KindBadElement, el.HeaderOffset, perr.Error())
			return nil
		}
		blk.Lace = Lace{Kind: "fixed", Frames: n, Sizes: sizes}
		descLen = int64(used)
	}
	blk.FrameBytes = payloadLen - headerLen - descLen
	if blk.FrameBytes < 0 {
		b.report(fault.KindTruncated, el.HeaderOffset, "lacing descriptors exceed block payload")
		return nil
	}
	if blk.Lace.Frames > 1 {
		var sum int64
		for i, s := range blk.Lace.Sizes {
			if s < 0 {
				blk.Lace.Sizes[i] = blk.FrameBytes - sum
				s = blk.Lace.Sizes[i]
			}
			if s < 0 {
				b.report(fault.KindBadElement, el.HeaderOffset, "lacing implies a negative final frame size")
				return nil
			}
			sum += s
		}
		if sum != blk.FrameBytes {
			b.report(fault.KindBadElement, el.HeaderOffset,
				fmt.Sprintf("lacing sizes sum to %d but payload holds %d", sum, blk.FrameBytes))
			return nil
		}
	}
	framesStart := headerLen + descLen
	digest, derr := b.digestPayload(el.PayloadOffset+framesStart, blk.FrameBytes)
	if derr != nil {
		return derr
	}
	blk.FrameDigest = digest
	t, terr := b.header.Time(track, b.clusterTick+int64(rel))
	if terr != nil {
		b.report(fault.KindBadElement, el.HeaderOffset, terr.Error())
		return nil
	}
	blk.Time = t
	if _, ok := b.header.Tracks[track]; !ok {
		f := fault.New(fault.ModuleEBML, fault.KindBadElement, fault.NewOffset(uint64(el.HeaderOffset)),
			fmt.Sprintf("block references undeclared track %d", track))
		f.OnTrack(track)
		if b.faults != nil {
			b.faults.Add(f)
		}
	}
	b.blocks = append(b.blocks, blk)
	if b.onBlock != nil {
		return b.onBlock(blk)
	}
	return nil
}

// digestPayload streams n bytes through CRC32 so that memory stays constant.
func (b *BlockReader) digestPayload(off, n int64) (uint32, error) {
	h := crc32.NewIEEE()
	const chunk = 32 * 1024
	var done int64
	for done < n {
		want := n - done
		if want > chunk {
			want = chunk
		}
		part, err := b.rd.ReadAt(off+done, want)
		if err != nil {
			return 0, err
		}
		if int64(len(part)) != want {
			return 0, fmt.Errorf("block payload ends early at 0x%x", off+done)
		}
		h.Write(part)
		done += want
	}
	return h.Sum32(), nil
}

// ParseTrackNumber decodes the block's track number vint and returns its
// encoded width.
func ParseTrackNumber(buf []byte) (track uint64, size uint64, width int, err error) {
	if len(buf) == 0 {
		return 0, 0, 0, fmt.Errorf("empty block")
	}
	n, e := vintLength(buf[0], 8)
	if e != nil {
		return 0, 0, 0, e
	}
	if len(buf) < n {
		return 0, 0, 0, fmt.Errorf("truncated track number")
	}
	v := uint64(buf[0]) & (0xFF >> uint(n))
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(buf[i])
	}
	return v, 0, n, nil
}

// parseXLace decodes Xiph (used>=1) or EBML lacing descriptors.  It returns
// the frame count, every frame size, and how many descriptor bytes were used.
func parseXLace(body []byte, ebml bool) (int, []int64, int, error) {
	if len(body) < 2 {
		return 0, nil, 0, fmt.Errorf("lacing header truncated")
	}
	n := int(body[0]) + 1
	if n > 256 {
		return 0, nil, 0, fmt.Errorf("lacing frame count %d exceeds limit", n)
	}
	sizes := make([]int64, 0, n)
	pos := 1
	var sum int64
	if ebml {
		first, used, err := parseUnsignedVINT(body[pos:])
		if err != nil {
			return 0, nil, 0, fmt.Errorf("ebml lacing first size invalid")
		}
		pos += used
		sizes = append(sizes, first)
		sum = first
		for i := 1; i < n-1; i++ {
			delta, used, err := parseSignedVINT(body[pos:])
			if err != nil {
				return 0, nil, 0, fmt.Errorf("ebml lacing delta %d truncated", i)
			}
			pos += used
			next := sizes[i-1] + delta
			if next < 0 {
				return 0, nil, 0, fmt.Errorf("ebml lacing frame %d has negative size", i)
			}
			sizes = append(sizes, next)
			sum += next
		}
	} else {
		for i := 0; i < n-1; i++ {
			var v int64
			for {
				if pos >= len(body) {
					return 0, nil, 0, fmt.Errorf("xiph lacing descriptor truncated")
				}
				c := body[pos]
				pos++
				v += int64(c)
				if c != 0xFF {
					break
				}
			}
			sizes = append(sizes, v)
			sum += v
		}
	}
	// Last frame size is implied by the remaining payload.
	_ = sum
	sizes = append(sizes, -1)
	return n, sizes, pos, nil
}

func parseFixedLace(body []byte, payload int64) (int, []int64, int, error) {
	if len(body) < 1 {
		return 0, nil, 0, fmt.Errorf("fixed lacing header truncated")
	}
	n := int(body[0]) + 1
	if n > 256 {
		return 0, nil, 0, fmt.Errorf("fixed lacing frame count %d exceeds limit", n)
	}
	rest := payload - 1
	if rest < 0 || rest%int64(n) != 0 {
		return 0, nil, 0, fmt.Errorf("fixed lacing payload %d does not divide into %d frames", rest, n)
	}
	sz := rest / int64(n)
	sizes := make([]int64, n)
	for i := range sizes {
		sizes[i] = sz
	}
	return n, sizes, 1, nil
}

// parseUnsignedVINT decodes a marker-stripped variable-length integer.
func parseUnsignedVINT(buf []byte) (int64, int, error) {
	if len(buf) == 0 {
		return 0, 0, ErrInvalidVINT
	}
	n, err := vintLength(buf[0], 8)
	if err != nil {
		return 0, 0, err
	}
	if len(buf) < n {
		return 0, 0, fmt.Errorf("ebml: truncated vint")
	}
	v := int64(buf[0]) & int64(0xFF>>uint(n))
	for i := 1; i < n; i++ {
		v = v<<8 | int64(buf[i])
	}
	return v, n, nil
}

func laceName(l byte) string {
	if l == 1 {
		return "xiph"
	}
	return "ebml"
}

func payloadOf(r *Reader, el Element, max int64) []byte {
	raw, _ := r.ReadPayload(el, max)
	return raw
}

func readUint(raw []byte) (uint64, bool) {
	if len(raw) == 0 || len(raw) > 8 {
		return 0, false
	}
	var v uint64
	for _, c := range raw {
		v = v<<8 | uint64(c)
	}
	return v, true
}

func readFloat(raw []byte) (float64, bool) {
	switch len(raw) {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(raw))), true
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(raw)), true
	}
	return 0, false
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
