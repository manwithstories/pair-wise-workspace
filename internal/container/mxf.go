package container

import (
	"bytes"
	"encoding/binary"
	"strings"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/model"
)

// MXF UL keys used by this build. The parser recognises the SMPTE universal
// label prefix and treats everything else as an unknown KLV triplet.
var (
	mxfPartitionPrefix = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01}
	mxfPrimerKey       = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x05, 0x01, 0x00}
	mxfIndexTableKey   = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x10, 0x01, 0x00}
	mxfTrackSetKey     = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x7f, 0x01, 0x00}
	mxfPictureEssence  = []byte{0x06, 0x0e, 0x2b, 0x34, 0x01, 0x02, 0x01, 0x01, 0x0d, 0x01, 0x03, 0x01, 0x15, 0x01, 0x05, 0x00}
	mxfFillKey         = []byte{0x06, 0x0e, 0x2b, 0x34, 0x01, 0x01, 0x01, 0x02, 0x06, 0x01, 0x01, 0x10, 0x01, 0x00, 0x00, 0x00}
)

// MXF local-set tags used by the relay track descriptor and index table
// segment. 0x3fxx tags are index properties; 0x80xx container description;
// 0x90xx bitstream sample description; 0xa0xx timing.
const (
	tagTrackID         = 0x8001
	tagKind            = 0x8002
	tagCodec           = 0x8003
	tagEditNum         = 0x8004
	tagEditDen         = 0x8005
	tagDuration        = 0x8006
	tagContainerRot    = 0x8007
	tagBitRot          = 0x8008
	tagContainerCrop   = 0x8009
	tagBitCrop         = 0x800a
	tagContainerPrim   = 0x800b
	tagContainerXfer   = 0x800c
	tagContainerMat    = 0x800d
	tagContainerRange  = 0x800e
	tagBitPrim         = 0x800f
	tagBitXfer         = 0x8010
	tagBitMat          = 0x8011
	tagBitRange        = 0x8012
	tagContainerMarker = 0x8013
	tagBitMarker       = 0x8014
	tagContainerW      = 0x8015
	tagContainerH      = 0x8016
	tagBitW            = 0x8017
	tagBitH            = 0x8018
	tagBitAperture     = 0x8019
	tagStartPos        = 0x3f01
	tagIndexCount      = 0x3f02
	tagIndexEntries    = 0x3f03
)

// IndexEntry is one MXF index table entry: the stream offset of the sample
// payload relative to the start of its KLV value, its duration in edit units,
// its edit-unit offset and whether it is a random-access point.
type IndexEntry struct {
	StreamOffset  uint32
	Duration      uint32
	EditUnitDelta uint32
	Key           bool
}

// mxfEntrySize is the fixed on-disk size of one index entry.
const mxfEntrySize = 16

type mxfParser struct {
	name    string
	data    []byte
	lim     Limits
	faults  *fault.List
	file    *File
	triplet int
	tracks  map[uint32]*mxfTrackState
	order   []uint32 // track IDs in metadata-set order
	// essence holds KLV picture items in file order.
	essence []mxfEssence
	// indexEntries maps track ID to parsed entries.
	indexEntries map[uint32][]IndexEntry
	indexSeen    map[uint32]bool
}

type mxfTrackState struct {
	track    *model.Track
	editNum  int64
	editDen  int64
	duration int64
	start    int64
	pending  map[uint16]string
}

type mxfEssence struct {
	trackID uint32
	off     int64
	size    int64
}

// ParseMXF parses an MXF (KLV) container. Every KLV triplet is length-checked
// against the remaining bytes and the configured KLV cap; violations are
// reported with the triplet's byte offset.
func ParseMXF(name string, data []byte, lim Limits) (*File, error) {
	l, err := lim.Normalize()
	if err != nil {
		return nil, fault.New(-1, 0, fault.KindLimit, false, "%v", err)
	}
	p := &mxfParser{
		name:         name,
		data:         data,
		lim:          l,
		faults:       &fault.List{},
		file:         &File{Name: name, Kind: model.MXF, Data: data, Faults: &fault.List{}},
		tracks:       map[uint32]*mxfTrackState{},
		indexEntries: map[uint32][]IndexEntry{},
		indexSeen:    map[uint32]bool{},
	}
	if int64(len(data)) > l.MaxFileSize {
		p.faults.Add(fault.New(-1, 0, fault.KindLimit, false,
			"file size %d exceeds limit %d", len(data), l.MaxFileSize))
	} else {
		p.walk()
	}
	p.buildTracks()
	p.file.Faults = p.faults
	for i, t := range p.file.Tracks {
		t.Number = i
	}
	if fe := p.faults.Fatal(); fe != nil {
		return p.file, fe
	}
	return p.file, nil
}

// walk iterates KLV triplets.
func (p *mxfParser) walk() {
	pos := int64(0)
	haveHeader := false
	for pos < int64(len(p.data)) {
		if p.triplet >= p.lim.MaxBoxes {
			p.faults.Add(fault.New(-1, pos, fault.KindLimit, false,
				"KLV triplet count exceeds limit %d", p.lim.MaxBoxes))
			return
		}
		if int64(len(p.data))-pos < 17 {
			p.faults.Add(fault.New(-1, pos, fault.KindTruncated, false,
				"%d trailing bytes at offset %d cannot form a KLV triplet", int64(len(p.data))-pos, pos))
			return
		}
		key := p.data[pos : pos+16]
		length, n, ferr := readBERLength(p.data[pos+16:], pos+16)
		if ferr != nil {
			p.faults.Add(ferr)
			return
		}
		valueStart := pos + 16 + int64(n)
		if int64(length) > p.lim.MaxKLV {
			p.faults.Add(fault.New(-1, pos, fault.KindLimit, false,
				"KLV at offset %d declares %d bytes, exceeding limit %d", pos, length, p.lim.MaxKLV))
			return
		}
		if int64(length) > int64(len(p.data))-valueStart {
			p.faults.Add(fault.New(-1, pos, fault.KindTruncated, false,
				"KLV at offset %d declares %d bytes but only %d remain",
				pos, length, int64(len(p.data))-valueStart))
			return
		}
		p.triplet++
		value := p.data[valueStart : valueStart+int64(length)]
		switch {
		case isPartition(key):
			haveHeader = true
			p.checkPartition(pos, key, value)
		case bytes.Equal(key, mxfPrimerKey), bytes.Equal(key, mxfFillKey):
			// No payload semantics required for this build.
		case bytes.Equal(key, mxfTrackSetKey):
			p.parseTrackSet(pos, value)
		case bytes.Equal(key, mxfIndexTableKey):
			p.parseIndexTable(pos, value)
		case bytes.Equal(key, mxfPictureEssence):
			p.parseEssence(pos, valueStart, value)
		default:
			if !haveHeader {
				p.faults.Add(fault.New(-1, pos, fault.KindCorrupt, true,
					"KLV at offset %d appears before the header partition", pos))
			}
			// Unknown but well-framed triplets are skipped deliberately.
		}
		pos = valueStart + int64(length)
	}
}

// readBERLength decodes a BER length at absolute offset off and returns the
// number of bytes consumed.
func readBERLength(buf []byte, off int64) (uint64, int, *fault.Error) {
	if len(buf) < 1 {
		return 0, 0, fault.New(-1, off, fault.KindTruncated, false, "BER length missing")
	}
	b := buf[0]
	if b < 0x80 {
		return uint64(b), 1, nil
	}
	n := int(b & 0x7f)
	if n == 0 {
		return 0, 0, fault.New(-1, off, fault.KindCorrupt, false,
			"BER indefinite length is not permitted in MXF")
	}
	if n > 8 || len(buf) < 1+n {
		return 0, 0, fault.New(-1, off, fault.KindTruncated, false,
			"BER long-form length needs %d bytes", n)
	}
	var v uint64
	for i := 0; i < n; i++ {
		v = v<<8 | uint64(buf[1+i])
	}
	if v > uint64(^uint64(0)>>1) {
		return 0, 0, fault.New(-1, off, fault.KindOutOfRange, false,
			"BER length %d exceeds signed range", v)
	}
	return v, 1 + n, nil
}

func isPartition(key []byte) bool {
	return len(key) == 16 && bytes.Equal(key[:13], mxfPartitionPrefix)
}

func (p *mxfParser) checkPartition(pos int64, key, value []byte) {
	// key[13] is the partition status, key[14] is 0x03 body, 0x04 header,
	// 0x05 footer in this build's profile.
	kind := key[14]
	if kind != 0x03 && kind != 0x04 && kind != 0x05 {
		p.faults.Add(fault.New(-1, pos, fault.KindCorrupt, true,
			"partition at offset %d has unknown position byte 0x%02x", pos, kind))
	}
	if len(value) < 4 {
		p.faults.Add(fault.New(-1, pos, fault.KindTruncated, true,
			"partition pack at offset %d is shorter than 4 bytes", pos))
	}
}

// localSet iterates the tag/length/value items of a local set payload.
func (p *mxfParser) localSet(base int64, payload []byte, fn func(tag uint16, val []byte, off int64) *fault.Error) {
	pos := 0
	for pos < len(payload) {
		if len(payload)-pos < 3 {
			p.faults.Add(fault.New(-1, base+int64(pos), fault.KindTruncated, true,
				"%d trailing bytes in local set at offset %d", len(payload)-pos, base+int64(pos)))
			return
		}
		tag := binary.BigEndian.Uint16(payload[pos : pos+2])
		length, n, ferr := readBERLength(payload[pos+2:], base+int64(pos+2))
		if ferr != nil {
			p.faults.Add(ferr)
			return
		}
		vstart := pos + 2 + n
		if int64(length) > int64(len(payload))-int64(vstart) {
			p.faults.Add(fault.New(-1, base+int64(pos), fault.KindTruncated, false,
				"local set item 0x%04x at offset %d declares %d bytes but only %d remain",
				tag, base+int64(pos), length, int64(len(payload))-int64(vstart)))
			return
		}
		if err := fn(tag, payload[vstart:vstart+int(length)], base+int64(pos)); err != nil {
			p.faults.Add(err)
		}
		pos = vstart + int(length)
	}
}

func (p *mxfParser) parseTrackSet(pos int64, value []byte) {
	st := &mxfTrackState{
		track: &model.Track{
			Container: model.MXF,
			Claims:    model.Claims{Policy: map[string]string{}, Bitstream: map[string]string{}, Container: map[string]string{}},
		},
		pending: map[uint16]string{},
	}
	var editNum, editDen int64
	p.localSet(pos, value, func(tag uint16, v []byte, off int64) *fault.Error {
		switch tag {
		case tagTrackID:
			if len(v) != 4 {
				return fault.New(-1, off, fault.KindCorrupt, true, "track ID field has %d bytes", len(v))
			}
			st.track.ID = binary.BigEndian.Uint32(v)
		case tagKind:
			st.track.Kind = model.TrackKind(strings.TrimSpace(string(v)))
		case tagCodec:
			st.track.Codec = model.Codec(strings.TrimSpace(string(v)))
		case tagEditNum:
			editNum = int64(binary.BigEndian.Uint32(v))
		case tagEditDen:
			editDen = int64(binary.BigEndian.Uint32(v))
		case tagDuration:
			st.duration = int64(binary.BigEndian.Uint64(v))
		case tagStartPos:
			st.start = int64(binary.BigEndian.Uint64(v))
		default:
			dst, ok := claimTarget(tag)
			if !ok {
				return nil
			}
			s, err := decodeClaim(tag, v, off)
			if err != nil {
				return err
			}
			dst(st.track, s)
		}
		return nil
	})
	if st.track.ID == 0 {
		p.faults.Add(fault.New(-1, pos, fault.KindCorrupt, true,
			"track descriptor set at offset %d has no track ID", pos))
		return
	}
	if _, dup := p.tracks[st.track.ID]; dup {
		p.faults.Add(fault.New(-1, pos, fault.KindCorrupt, true,
			"duplicate track descriptor for ID %d", st.track.ID))
		return
	}
	if editNum <= 0 {
		editNum = 25
	}
	if editDen <= 0 {
		editDen = 1
	}
	tb, err := model.NewTimebase(editNum, editDen)
	if err != nil {
		p.faults.Add(fault.Wrap(-1, pos, fault.KindCorrupt, false, err))
		return
	}
	st.editNum, st.editDen = editNum, editDen
	st.track.Timebase = tb
	st.track.Duration = st.duration
	if st.track.Kind == "" {
		st.track.Kind = model.Data
	}
	p.tracks[st.track.ID] = st
	p.order = append(p.order, st.track.ID)
	if len(p.order) > p.lim.MaxTracks {
		p.faults.Add(fault.New(-1, pos, fault.KindLimit, false,
			"track count exceeds limit %d", p.lim.MaxTracks))
	}
}
