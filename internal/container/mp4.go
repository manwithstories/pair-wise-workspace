package container

import (
	"bytes"
	"encoding/binary"

	"github.com/abema/go-mp4"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/model"
)

// mp4Parser walks the ISO BMFF box tree with explicit bounds checking. The
// abema/go-mp4 package decodes typed box payloads; this walker owns structural
// traversal so truncation and out-of-range lengths surface with their exact
// byte offsets rather than as generic errors.
type mp4Parser struct {
	name   string
	data   []byte
	lim    Limits
	faults *fault.List
	file   *File
	boxes  int
}

// ParseMP4 parses an ISO BMFF (MP4) container. It always returns a File
// carrying every fault found; the returned error is the first non-recoverable
// fault.
func ParseMP4(name string, data []byte, lim Limits) (*File, error) {
	l, err := lim.Normalize()
	if err != nil {
		return nil, fault.New(-1, 0, fault.KindLimit, false, "%v", err)
	}
	p := &mp4Parser{
		name:   name,
		data:   data,
		lim:    l,
		faults: &fault.List{},
		file:   &File{Name: name, Kind: model.MP4, Data: data, Faults: &fault.List{}},
	}
	switch {
	case int64(len(data)) > l.MaxFileSize:
		p.faults.Add(fault.New(-1, 0, fault.KindLimit, false,
			"file size %d exceeds limit %d", len(data), l.MaxFileSize))
	case len(data) < 8:
		p.faults.Add(fault.New(-1, 0, fault.KindTruncated, false,
			"file size %d is smaller than a box header", len(data)))
	default:
		p.walk(0, int64(len(data)), 0, nil)
	}
	p.file.Faults = p.faults
	for i, t := range p.file.Tracks {
		t.Number = i
	}
	if fe := p.faults.Fatal(); fe != nil {
		return p.file, fe
	}
	return p.file, nil
}

// box is a validated box header.
type box struct {
	typ    string
	offset int64
	size   int64
	header int64
}

// trakState accumulates the tables of one trak until the whole box tree has
// been visited; sample offsets cannot be resolved until stco/stsc/stsz are all
// known, regardless of their on-disk order.
type trakState struct {
	track        *model.Track
	kind         model.TrackKind
	chunkOffsets []uint32
	stsc         []mp4.StscEntry
	syncSet      map[int]bool
	hadStts      bool
	hadStsz      bool
	hadStco      bool
}

// walk visits boxes in [start,end) and descends into structural boxes.
func (p *mp4Parser) walk(start, end int64, depth int, st *trakState) {
	if depth > p.lim.MaxDepth {
		p.faults.Add(fault.New(-1, start, fault.KindLimit, false,
			"box nesting at offset %d exceeds depth limit %d", start, p.lim.MaxDepth))
		return
	}
	pos := start
	for pos < end {
		if p.boxes >= p.lim.MaxBoxes {
			p.faults.Add(fault.New(-1, pos, fault.KindLimit, false,
				"box count exceeds limit %d", p.lim.MaxBoxes))
			return
		}
		b, ferr := p.readBox(pos, end)
		if ferr != nil {
			p.faults.Add(ferr)
			return
		}
		p.boxes++
		payload := pos + b.header
		payloadEnd := pos + b.size
		switch b.typ {
		case "trak":
			if len(p.file.Tracks) >= p.lim.MaxTracks {
				p.faults.Add(fault.New(-1, b.offset, fault.KindLimit, false,
					"track count exceeds limit %d", p.lim.MaxTracks))
				return
			}
			child := &trakState{
				track: &model.Track{Container: model.MP4, Kind: model.Data,
					Claims: model.Claims{Policy: map[string]string{}, Bitstream: map[string]string{}, Container: map[string]string{}}},
				syncSet: map[int]bool{},
			}
			p.file.Tracks = append(p.file.Tracks, child.track)
			p.walk(payload, payloadEnd, depth+1, child)
			p.finalize(child)
			if child.track.ID == 0 {
				p.faults.Add(fault.New(-1, b.offset, fault.KindCorrupt, true,
					"trak without a valid tkhd track ID"))
			}
		case "moov", "mdia", "minf", "stbl", "edts", "dinf", "udta", "mvex":
			p.walk(payload, payloadEnd, depth+1, st)
		case "ftyp":
			p.parseFtyp(b, payload, payloadEnd)
		case "hdlr":
			if st != nil {
				st.kind = p.parseHdlr(b, payload, payloadEnd)
			}
		case "tkhd":
			if st != nil {
				p.parseTkhd(b, payload, payloadEnd, st.track)
			}
		case "mdhd":
			if st != nil {
				p.parseMdhd(b, payload, payloadEnd, st.track)
			}
		case "colr":
			if st != nil {
				p.parseColr(b, payload, payloadEnd, st.track, model.SourceContainer)
			}
		case "stsd":
			if st != nil {
				p.parseStsd(b, payload, payloadEnd, st)
			}
		case "stts":
			if st != nil {
				p.parseStts(b, payload, payloadEnd, st)
			}
		case "stsz":
			if st != nil {
				p.parseStsz(b, payload, payloadEnd, st)
			}
		case "stco":
			if st != nil {
				p.parseStco(b, payload, payloadEnd, st)
			}
		case "stsc":
			if st != nil {
				p.parseStsc(b, payload, payloadEnd, st)
			}
		case "stss":
			if st != nil {
				p.parseStss(b, payload, payloadEnd, st)
			}
		case "mdat":
			// Essence is addressed by absolute offset from the sample tables.
		}
		pos = payloadEnd
	}
}

// readBox reads and validates a box header at pos.
func (p *mp4Parser) readBox(pos, end int64) (box, *fault.Error) {
	if end-pos < 8 {
		return box{}, fault.New(-1, pos, fault.KindTruncated, false,
			"%d trailing bytes at offset %d cannot form a box header", end-pos, pos)
	}
	hdr := p.data[pos : pos+8]
	size := int64(binary.BigEndian.Uint32(hdr[0:4]))
	typ := string(hdr[4:8])
	header := int64(8)
	if size == 1 {
		if end-pos < 16 {
			return box{}, fault.New(-1, pos, fault.KindTruncated, false,
				"box %q large-size header truncated", typ)
		}
		size = int64(binary.BigEndian.Uint64(p.data[pos+8 : pos+16]))
		header = 16
	} else if size == 0 {
		size = end - pos
	}
	if size < header {
		return box{}, fault.New(-1, pos, fault.KindCorrupt, false,
			"box %q declares size %d, smaller than its %d-byte header", typ, size, header)
	}
	if pos+size > end || pos+size > int64(len(p.data)) {
		limit := end
		if int64(len(p.data)) < limit {
			limit = int64(len(p.data))
		}
		return box{}, fault.New(-1, pos, fault.KindTruncated, false,
			"box %q at offset %d declares %d bytes but only %d remain", typ, pos, size, limit-pos)
	}
	return box{typ: typ, offset: pos, size: size, header: header}, nil
}

// unmarshal decodes a typed box payload into dst. Unmarshal (rather than
// UnmarshalAny) is required because the latter allocates its own box and
// discards the caller's destination.
func (p *mp4Parser) unmarshal(payload []byte, dst mp4.IBox) error {
	_, err := mp4.Unmarshal(bytes.NewReader(payload), uint64(len(payload)), dst, mp4.Context{})
	return err
}
