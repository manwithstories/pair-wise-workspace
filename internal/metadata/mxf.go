package metadata

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"

	"mediamerge/internal/clock"
	"mediamerge/internal/fault"
)

// The MXF fixture is a deliberately small, fully specified KLV subset:
//
//	header partition  (partition pack + primer + index table segment)
//	body partition    (essence element KLV per edit unit)
//	footer partition  (partition pack + index table segment)
//
// Each KLV triple is a 16-byte UL, a BER length and a value. Track
// descriptions live in scanline-ordered, fixed-size metadata items so that a
// truncated partition is detectable from the partition byte count alone.
// Nothing here claims conformance to a particular operational pattern; it is
// a self-consistent carrier for the fields the pipeline needs.

// isPartitionKey reports whether a UL is any partition pack flavour. The
// fixture writes one of three fixed keys, but the parser accepts all three so
// a hand-built file cannot bypass partition validation.
func isPartitionKey(key []byte) bool {
	for _, k := range []string{partHdrUL, partBodyUL, partFootUL} {
		if bytes.Equal(key, ulBytes(k)) {
			return true
		}
	}
	return false
}

const (
	mxfKeyLen    = 16
	mxfHeaderLen = 20 // 16-byte UL + 4-byte BER long-form length

	partitionPackUL = "060e2b34020501010d01020101020000"
	partHdrUL       = "060e2b34020501010d01020101020000"
	partBodyUL      = "060e2b34020501010d01020101020300"
	partFootUL      = "060e2b34020501010d01020101020400"
	primerKey       = "060e2b34020501010d01020101050000"
	indexTableKey   = "060e2b34020501010d01020101110000"
	essenceKey      = "060e2b34010201010d0103011b000000"
	metadataKey     = "060e2b34020501010d010201017f0000"
)

// MXFDocument is the generator-side description of one MXF file.
type MXFDocument struct {
	// OperationalPattern is carried verbatim in the partition pack.
	OperationalPattern string
	Tracks             []MXFTrack
	// EditUnitBytes is the essence size per track per edit unit.
	EditUnitBytes int
	EditUnits     int
	// IndexInterval is the number of edit units between index entries.
	IndexInterval  int
	ContainerColor *ColorDesc
}

// MXFTrack is one track description in the MXF metadata.
type MXFTrack struct {
	ID         int
	Kind       Kind
	Timebase   clock.Timebase
	Duration   int64
	Width      int
	Height     int
	StoredRot  int // rotation as stored on the track, can be an odd angle
	Crop       *Crop
	Color      ColorDesc
	SampleRate int
	Channels   int
	// TimestampTicks are per-edit-unit presentation ticks. A generator may
	// deliberately emit non-monotonic values for the negative fixture.
	TimestampTicks []int64
	// KeyEditUnits are 0-based edit unit numbers flagged as key frames.
	KeyEditUnits []int
}

func ulBytes(hex string) []byte {
	out := make([]byte, mxfKeyLen)
	for i := 0; i < mxfKeyLen; i++ {
		var b byte
		fmt.Sscanf(hex[2*i:2*i+2], "%02x", &b)
		out[i] = b
	}
	return out
}

// klv encodes a KLV triple with a 4-byte BER length: 0x83 followed by a
// 3-byte big-endian value.
func klv(key string, value []byte) []byte {
	out := make([]byte, mxfHeaderLen+len(value))
	copy(out, ulBytes(key))
	out[16] = 0x83
	out[17] = byte(len(value) >> 16)
	out[18] = byte(len(value) >> 8)
	out[19] = byte(len(value))
	copy(out[mxfHeaderLen:], value)
	return out
}

func putU16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }
func putU32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }
func putU64(b []byte, v uint64) { binary.BigEndian.PutUint64(b, v) }

// item is a fixed-size metadata item: 4-byte tag, 4-byte length, value.
func item(tag uint32, value []byte) []byte {
	out := make([]byte, 8, 8+len(value))
	putU32(out, tag)
	putU32(out[4:], uint32(len(value)))
	return append(out, value...)
}

const (
	tagTrackID    = 0x3c0a
	tagKind       = 0x3c0b
	tagTimebaseNu = 0x3c0c
	tagTimebaseDe = 0x3c0d
	tagDuration   = 0x3c0e
	tagWidth      = 0x3c0f
	tagHeight     = 0x3c10
	tagRotation   = 0x3c11
	tagPrimaries  = 0x3c12
	tagTransfer   = 0x3c13
	tagMatrix     = 0x3c14
	tagRange      = 0x3c15
	tagSampleRate = 0x3c16
	tagChannels   = 0x3c17
	tagCropX      = 0x3c18
	tagCropY      = 0x3c19
	tagCropW      = 0x3c1a
	tagCropH      = 0x3c1b
	tagTimestamps = 0x3c1c
	tagKeyUnits   = 0x3c1d
	// tagContainer* only appear in the header partition.
	tagContPrimaries = 0x3d01
	tagContTransfer  = 0x3d02
	tagContMatrix    = 0x3d03
	tagContRange     = 0x3d04
)

// Write serializes the document.
func (doc *MXFDocument) Write() ([]byte, error) {
	if len(doc.Tracks) == 0 {
		return nil, fmt.Errorf("metadata: mxf needs at least one track")
	}
	if doc.EditUnits <= 0 || doc.EditUnitBytes <= 0 {
		return nil, fmt.Errorf("metadata: mxf needs positive edit units")
	}
	if doc.IndexInterval <= 0 {
		doc.IndexInterval = 1
	}
	if doc.OperationalPattern == "" {
		doc.OperationalPattern = "OP1a"
	}

	meta, err := doc.metadataSet()
	if err != nil {
		return nil, err
	}
	index, err := doc.indexTable()
	if err != nil {
		return nil, err
	}

	header := doc.partitionPack(partHdrUL, 0, 0, 0)
	primer := klv(primerKey, []byte{1, 0, 0, 0})
	headerBody := concat(header, primer, klv(metadataKey, meta), index)

	var body bytes.Buffer
	for u := 0; u < doc.EditUnits; u++ {
		for ti := range doc.Tracks {
			slot := bytes.Repeat([]byte{byte('0' + ti)}, doc.EditUnitBytes)
			binary.BigEndian.PutUint32(slot, uint32(u))
			body.Write(klv(essenceKey, slot))
		}
	}
	bodyBytes := body.Bytes()
	seq := uint32(doc.EditUnits * len(doc.Tracks))
	bodyPack := doc.partitionPack(partBodyUL, len(headerBody), uint64(len(headerBody)), seq+uint32(len(headerBody)))

	footerIndex, err := doc.indexTable()
	if err != nil {
		return nil, err
	}
	footerPack := doc.partitionPack(partFootUL, len(headerBody)+len(bodyPack)+len(bodyBytes),
		uint64(len(headerBody)+len(bodyPack)+len(bodyBytes)), seq*2)
	footer := concat(footerPack, footerIndex)

	out := concat(headerBody, bodyPack, bodyBytes, footer)
	return out, nil
}

func (doc *MXFDocument) partitionPack(key string, prev int, bodyOffset uint64, seq uint32) []byte {
	v := make([]byte, 64)
	copy(v[0:4], []byte("MXF\x01"))
	putU16(v[4:], 1)
	putU16(v[6:], 1)
	copy(v[8:12], []byte("mm\x00\x00"))
	copy(v[12:16], []byte(doc.OperationalPattern))
	putU32(v[16:], uint32(seq))
	// Base offset, body offset, footer offset are all relative to the
	// file start in this fixture; only the body offset is used.
	putU64(v[24:], bodyOffset)
	putU32(v[32:], uint32(doc.EditUnits))
	putU32(v[36:], uint32(len(doc.Tracks)))
	if prev > 0 {
		putU32(v[40:], uint32(prev))
	}
	return klv(key, v)
}

func (doc *MXFDocument) metadataSet() ([]byte, error) {
	var out []byte
	if doc.ContainerColor != nil {
		d := doc.ContainerColor
		if d.Primaries != nil {
			out = append(out, item(tagContPrimaries, u16v(*d.Primaries))...)
		}
		if d.Transfer != nil {
			out = append(out, item(tagContTransfer, u16v(*d.Transfer))...)
		}
		if d.Matrix != nil {
			out = append(out, item(tagContMatrix, u16v(*d.Matrix))...)
		}
		if d.FullRange != nil {
			out = append(out, item(tagContRange, []byte{boolByte(*d.FullRange)})...)
		}
	}
	for _, tr := range doc.Tracks {
		out = append(out, item(tagTrackID, u32v(uint32(tr.ID)))...)
		out = append(out, item(tagKind, []byte(tr.Kind))...)
		out = append(out, item(tagTimebaseNu, u32v(uint32(tr.Timebase.Rate.Num)))...)
		out = append(out, item(tagTimebaseDe, u32v(uint32(tr.Timebase.Rate.Den)))...)
		out = append(out, item(tagDuration, u64v(uint64(tr.Duration)))...)
		out = append(out, item(tagRotation, i32v(int32(tr.StoredRot)))...)
		if tr.Kind == KindVideo {
			out = append(out, item(tagWidth, u32v(uint32(tr.Width)))...)
			out = append(out, item(tagHeight, u32v(uint32(tr.Height)))...)
			for _, pair := range []struct {
				tag uint32
				v   *uint16
			}{{tagPrimaries, tr.Color.Primaries}, {tagTransfer, tr.Color.Transfer}, {tagMatrix, tr.Color.Matrix}} {
				if pair.v != nil {
					out = append(out, item(pair.tag, u16v(*pair.v))...)
				}
			}
			if tr.Color.FullRange != nil {
				out = append(out, item(tagRange, []byte{boolByte(*tr.Color.FullRange)})...)
			}
			if tr.Crop != nil {
				out = append(out,
					item(tagCropX, u32v(uint32(tr.Crop.X)))...)
				out = append(out, item(tagCropY, u32v(uint32(tr.Crop.Y)))...)
				out = append(out, item(tagCropW, u32v(uint32(tr.Crop.Width)))...)
				out = append(out, item(tagCropH, u32v(uint32(tr.Crop.Height)))...)
			}
		} else {
			out = append(out, item(tagSampleRate, u32v(uint32(tr.SampleRate)))...)
			out = append(out, item(tagChannels, u32v(uint32(tr.Channels)))...)
		}
		ts := make([]byte, 8*len(tr.TimestampTicks))
		for i, t := range tr.TimestampTicks {
			putU64(ts[8*i:], uint64(t))
		}
		out = append(out, item(tagTimestamps, ts)...)
		ku := make([]byte, 4*len(tr.KeyEditUnits))
		for i, k := range tr.KeyEditUnits {
			putU32(ku[4*i:], uint32(k))
		}
		out = append(out, item(tagKeyUnits, ku)...)
	}
	return out, nil
}

func (doc *MXFDocument) indexTable() ([]byte, error) {
	var out []byte
	for _, tr := range doc.Tracks {
		entries := len(tr.TimestampTicks)
		if entries == 0 {
			entries = doc.EditUnits
		}
		v := make([]byte, 16+8*entries)
		putU32(v, uint32(tr.ID))
		putU32(v[4:], uint32(doc.IndexInterval))
		putU32(v[8:], uint32(entries))
		putU32(v[12:], uint32(len(tr.KeyEditUnits)))
		for i := 0; i < entries; i++ {
			var tick uint64
			if i < len(tr.TimestampTicks) {
				tick = uint64(tr.TimestampTicks[i])
			} else {
				tick = uint64(i) * uint64(tr.Timebase.Rate.Den) * 1000 / uint64(tr.Timebase.Rate.Num)
			}
			putU64(v[16+8*i:], tick)
		}
		out = append(out, klv(indexTableKey, v)...)
	}
	return out, nil
}

func u16v(v uint16) []byte { b := make([]byte, 2); putU16(b, v); return b }
func u32v(v uint32) []byte { b := make([]byte, 4); putU32(b, v); return b }
func u64v(v uint64) []byte { b := make([]byte, 8); putU64(b, v); return b }
func i32v(v int32) []byte  { return u32v(uint32(v)) }

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}

func concat(parts ...[]byte) []byte {
	var n int
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// ParseMXF reads the fixture format and validates every partition and KLV
// boundary. Truncation is reported with the partition offset and byte count.
func ParseMXF(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fault.Wrap(fault.StageParse, fault.CodeTruncatedPartition, err, path)
	}
	p := &mxfParser{data: data}
	if err := p.parse(); err != nil {
		return nil, err
	}
	return p.file, nil
}

type mxfParser struct {
	data []byte
	file *File
	// pending tracks keyed by id
	tracks map[int]*mxfTrack
	order  []int
}

type mxfTrack struct {
	Track
	TimestampTicks []int64
}

func (p *mxfParser) parse() error {
	p.file = &File{Container: "mxf"}
	p.tracks = map[int]*mxfTrack{}
	off := 0
	for off < len(p.data) {
		if len(p.data)-off < mxfHeaderLen {
			return fault.New(fault.StageParse, fault.CodeTruncatedPartition,
				fmt.Sprintf("mxf: %d trailing bytes at offset %d cannot hold a KLV header", len(p.data)-off, off))
		}
		key := p.data[off : off+16]
		length, n, err := readBER(p.data[off+16:])
		if err != nil {
			return fault.New(fault.StageParse, fault.CodeTruncatedPartition,
				fmt.Sprintf("mxf: bad BER length at offset %d: %v", off, err))
		}
		valueStart := off + 16 + n
		valueEnd := valueStart + length
		if valueEnd > len(p.data) {
			return fault.New(fault.StageParse, fault.CodeTruncatedPartition,
				fmt.Sprintf("mxf: KLV at offset %d declares %d payload bytes but file ends at %d",
					off, length, len(p.data)))
		}
		value := p.data[valueStart:valueEnd]
		switch {
		case isPartitionKey(key):
			if err := p.readPartition(off, value); err != nil {
				return err
			}
		case bytes.Equal(key, ulBytes(indexTableKey)):
			if err := p.readIndex(value); err != nil {
				return err
			}
		case bytes.Equal(key, ulBytes(metadataKey)):
			if err := p.readMetadata(value); err != nil {
				return err
			}
		}
		off = valueEnd
	}
	if off != len(p.data) {
		return fault.New(fault.StageParse, fault.CodeTruncatedPartition,
			fmt.Sprintf("mxf: KLV walk ended at %d of %d bytes", off, len(p.data)))
	}
	for _, id := range p.order {
		mt := p.tracks[id]
		p.file.Tracks = append(p.file.Tracks, &mt.Track)
	}
	return nil
}

func (p *mxfParser) readPartition(off int, value []byte) error {
	if len(value) < 64 {
		return fault.New(fault.StageParse, fault.CodeTruncatedPartition,
			fmt.Sprintf("mxf: partition pack at offset %d is %d bytes, need 64", off, len(value)))
	}
	if !bytes.Equal(value[0:4], []byte("MXF\x01")) {
		return fault.New(fault.StageParse, fault.CodeTruncatedPartition,
			fmt.Sprintf("mxf: partition at offset %d has bad magic", off))
	}
	bodyOffset := binary.BigEndian.Uint64(value[24:])
	if bodyOffset != 0 && bodyOffset > uint64(len(p.data)) {
		return fault.New(fault.StageParse, fault.CodeTruncatedPartition,
			fmt.Sprintf("mxf: partition at offset %d points to body offset %d beyond EOF %d",
				off, bodyOffset, len(p.data)))
	}
	return nil
}

func (p *mxfParser) readIndex(value []byte) error {
	if len(value) < 16 {
		return fault.New(fault.StageParse, fault.CodeTruncatedPartition, "mxf: index table segment too short")
	}
	id := int(binary.BigEndian.Uint32(value))
	n := int(binary.BigEndian.Uint32(value[8:]))
	if 16+8*n > len(value) {
		return fault.New(fault.StageParse, fault.CodeTruncatedPartition,
			fmt.Sprintf("mxf: index for track %d declares %d entries in %d bytes", id, n, len(value)))
	}
	mt := p.tracks[id]
	if mt == nil {
		return fault.NewTrack(fault.StageParse, fault.CodeParseBox, id, "mxf: index precedes metadata")
	}
	// Index entries repeat the timestamps; the metadata set is
	// authoritative, so only the entry count is validated here.
	if got := len(mt.TimestampTicks); got != n {
		return fault.NewTrack(fault.StageParse, fault.CodeParseBox, id,
			fmt.Sprintf("mxf: index has %d entries but metadata has %d timestamps", n, got))
	}
	return nil
}

func (p *mxfParser) readMetadata(value []byte) error {
	items, err := parseItems(value)
	if err != nil {
		return err
	}
	// Items are emitted track by track after the optional container colour
	// block. A new track starts whenever trackID reappears.
	var blocks []map[uint32][]byte
	var cur map[uint32][]byte
	var container *ColorDesc
	for _, it := range items.ordered {
		if it.tag >= tagContPrimaries && it.tag <= tagContRange {
			if container == nil {
				container = &ColorDesc{Source: "container"}
			}
			applyColor(container, it.tag, it.value)
			continue
		}
		if it.tag == tagTrackID {
			if cur != nil {
				blocks = append(blocks, cur)
			}
			cur = map[uint32][]byte{}
		}
		if cur == nil {
			return fault.New(fault.StageParse, fault.CodeParseBox,
				fmt.Sprintf("mxf: metadata item %#x appears before any track id", it.tag))
		}
		cur[it.tag] = it.value
	}
	if cur != nil {
		blocks = append(blocks, cur)
	}
	if container != nil {
		p.file.Color = container
	}
	for _, block := range blocks {
		if err := p.addTrack(block); err != nil {
			return err
		}
	}
	return nil
}

type itemEntry struct {
	tag   uint32
	value []byte
}

type itemSet struct {
	byTag   map[uint32][]byte
	ordered []itemEntry
}

func parseItems(value []byte) (*itemSet, error) {
	set := &itemSet{byTag: map[uint32][]byte{}}
	off := 0
	for off < len(value) {
		if len(value)-off < 8 {
			return nil, fault.New(fault.StageParse, fault.CodeTruncatedPartition,
				fmt.Sprintf("mxf: %d trailing metadata bytes at %d", len(value)-off, off))
		}
		tag := binary.BigEndian.Uint32(value[off:])
		n := int(binary.BigEndian.Uint32(value[off+4:]))
		if off+8+n > len(value) {
			return nil, fault.New(fault.StageParse, fault.CodeTruncatedPartition,
				fmt.Sprintf("mxf: metadata item %#x at %d declares %d bytes beyond set end", tag, off, n))
		}
		v := value[off+8 : off+8+n]
		set.byTag[tag] = v
		set.ordered = append(set.ordered, itemEntry{tag: tag, value: v})
		off += 8 + n
	}
	return set, nil
}

func applyColor(c *ColorDesc, tag uint32, v []byte) {
	switch {
	case tag == tagContPrimaries && len(v) == 2:
		x := binary.BigEndian.Uint16(v)
		c.Primaries = &x
	case tag == tagContTransfer && len(v) == 2:
		x := binary.BigEndian.Uint16(v)
		c.Transfer = &x
	case tag == tagContMatrix && len(v) == 2:
		x := binary.BigEndian.Uint16(v)
		c.Matrix = &x
	case tag == tagContRange && len(v) == 1:
		b := v[0] == 1
		c.FullRange = &b
	}
}

func (p *mxfParser) addTrack(items map[uint32][]byte) error {
	getU32 := func(tag uint32) (uint32, bool) {
		v, ok := items[tag]
		if !ok || len(v) != 4 {
			return 0, false
		}
		return binary.BigEndian.Uint32(v), true
	}
	getU64 := func(tag uint32) (uint64, bool) {
		v, ok := items[tag]
		if !ok || len(v) != 8 {
			return 0, false
		}
		return binary.BigEndian.Uint64(v), true
	}
	id32, ok := getU32(tagTrackID)
	if !ok {
		return fault.New(fault.StageParse, fault.CodeParseBox, "mxf: track without id")
	}
	id := int(id32)
	kindV, ok := items[tagKind]
	if !ok {
		return fault.NewTrack(fault.StageParse, fault.CodeParseBox, id, "mxf: track without kind")
	}
	kind := Kind(string(kindV))
	if kind != KindVideo && kind != KindAudio {
		return fault.NewTrack(fault.StageParse, fault.CodeParseBox, id, "mxf: unknown track kind "+string(kindV))
	}
	nu, _ := getU32(tagTimebaseNu)
	de, _ := getU32(tagTimebaseDe)
	tb, err := clock.New(int64(nu), int64(de))
	if err != nil {
		return fault.NewTrack(fault.StageParse, fault.CodeParseBox, id, "mxf: "+err.Error())
	}
	dur, _ := getU64(tagDuration)
	rot, _ := getU32(tagRotation)
	mt := &mxfTrack{Track: Track{ID: id, Kind: kind, Timebase: tb, DurationTicks: int64(dur)}}
	mt.Rotation = int(int32(rot))

	if kind == KindVideo {
		w, _ := getU32(tagWidth)
		h, _ := getU32(tagHeight)
		mt.Width, mt.Height = int(w), int(h)
		var fullRange *bool
		if v, ok := items[tagRange]; ok && len(v) == 1 {
			b := v[0] == 1
			fullRange = &b
		}
		desc := ColorDesc{FullRange: fullRange, Source: "track"}
		if v, ok := items[tagPrimaries]; ok && len(v) == 2 {
			x := binary.BigEndian.Uint16(v)
			desc.Primaries = &x
		}
		if v, ok := items[tagTransfer]; ok && len(v) == 2 {
			x := binary.BigEndian.Uint16(v)
			desc.Transfer = &x
		}
		if v, ok := items[tagMatrix]; ok && len(v) == 2 {
			x := binary.BigEndian.Uint16(v)
			desc.Matrix = &x
		}
		mt.Color = desc
		if _, ok := items[tagCropW]; ok {
			c, err := p.crop(items, id)
			if err != nil {
				return err
			}
			mt.Crop = c
		}
	} else {
		if v, ok := getU32(tagSampleRate); ok {
			mt.SampleRate = int(v)
		}
		if v, ok := getU32(tagChannels); ok {
			mt.Channels = int(v)
		}
	}

	// Timestamps and key units.
	if v, ok := items[tagTimestamps]; ok {
		if len(v)%8 != 0 {
			return fault.NewTrack(fault.StageParse, fault.CodeParseBox, id, "mxf: timestamp blob is not a multiple of 8")
		}
		var ticks []int64
		for i := 0; i < len(v); i += 8 {
			ticks = append(ticks, int64(binary.BigEndian.Uint64(v[i:])))
		}
		mt.TimestampTicks = ticks
	}
	var keys []int
	if v, ok := items[tagKeyUnits]; ok {
		if len(v)%4 != 0 {
			return fault.NewTrack(fault.StageParse, fault.CodeParseBox, id, "mxf: keyframe blob is not a multiple of 4")
		}
		for i := 0; i < len(v); i += 4 {
			keys = append(keys, int(binary.BigEndian.Uint32(v[i:]))+1)
		}
	}
	if len(mt.TimestampTicks) > 0 {
		if err := clock.CheckMonotonic(mt.TimestampTicks); err != nil {
			span := mt.Span()
			return fault.NewSpan(fault.StageParse, fault.CodeNonMonotonic, id, span,
				fmt.Sprintf("mxf timestamps: %v", err))
		}
		deltas := make([]int64, len(mt.TimestampTicks))
		var prev int64
		for i, t := range mt.TimestampTicks {
			deltas[i] = t - prev
			prev = t
		}
		// The first delta is the duration from zero; keep it positive.
		if deltas[0] <= 0 {
			deltas[0] = 1
		}
		idx, err := BuildKeyframeIndex(deltas, keys, tb)
		if err != nil {
			return fault.NewTrack(fault.StageParse, fault.CodeParseBox, id, err.Error())
		}
		mt.Keyframes = idx
	}
	p.tracks[id] = mt
	p.order = append(p.order, id)
	return nil
}

func (p *mxfParser) crop(items map[uint32][]byte, id int) (*Crop, error) {
	get := func(tag uint32) int {
		v, ok := items[tag]
		if !ok || len(v) != 4 {
			return 0
		}
		return int(binary.BigEndian.Uint32(v))
	}
	if _, ok := items[tagCropW]; !ok {
		return nil, nil
	}
	return &Crop{X: get(tagCropX), Y: get(tagCropY), Width: get(tagCropW), Height: get(tagCropH)}, nil
}

// readBER reads a BER length, supporting the 4-byte long form used here.
func readBER(b []byte) (length int, consumed int, err error) {
	if len(b) == 0 {
		return 0, 0, fmt.Errorf("empty length")
	}
	first := b[0]
	if first < 0x80 {
		return int(first), 1, nil
	}
	n := int(first & 0x7f)
	if n > 4 || len(b) < 1+n {
		return 0, 0, fmt.Errorf("unsupported BER length form")
	}
	v := 0
	for i := 0; i < n; i++ {
		v = v<<8 | int(b[1+i])
	}
	return v, 1 + n, nil
}
