package fixture

import (
	"bytes"
	"encoding/binary"

	"mediaarchive/internal/demux"
)

// box prepends an 8-byte 32-bit MP4 box header.
func box(typ string, payload []byte) []byte {
	total := uint32(8 + len(payload))
	out := make([]byte, 0, total)
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], total)
	copy(hdr[4:8], typ)
	out = append(out, hdr[:]...)
	return append(out, payload...)
}

func (b *builder) buildFtyp() []byte {
	var p bytes.Buffer
	if b.spec.QuickTime {
		p.WriteString("qt  ")
	} else {
		p.WriteString("isom")
	}
	var minor [4]byte
	p.Write(minor[:])
	brands := []string{"isom", "iso2", "mp41"}
	if b.spec.QuickTime {
		brands = append([]string{"qt  "}, brands...)
	}
	for _, br := range brands {
		p.WriteString(br)
	}
	return box("ftyp", p.Bytes())
}

// buildArmd emits armd; offset fields are zero placeholders patched later by
// resolveOffsets, keeping the serialized length fixed.
func (b *builder) buildArmd() []byte {
	var p []byte
	app := func(x []byte) { p = append(p, x...) }
	app([]byte("ARMD"))
	p = append(p, 1, 0)
	p = binary.BigEndian.AppendUint16(p, uint16(len(b.tracks)))
	for _, t := range b.tracks {
		p = binary.BigEndian.AppendUint32(p, t.ID)
		p = append(p, demux.KindByte(t.Kind))
		p = binary.BigEndian.AppendUint32(p, t.Timescale)
		p = binary.BigEndian.AppendUint64(p, t.StartTime)
		p = binary.BigEndian.AppendUint32(p, uint32(t.DriftPPM))
		p = binary.BigEndian.AppendUint32(p, t.WrapUnits)
		p = binary.BigEndian.AppendUint32(p, t.RTPCount)
		p = append(p, byte(len(t.CodecHint)))
		app([]byte(t.CodecHint))
	}
	p = binary.BigEndian.AppendUint32(p, uint32(len(b.tables)))
	b.offsetPos = b.offsetPos[:0]
	for ci, table := range b.tables {
		p = binary.BigEndian.AppendUint32(p, uint32(ci))
		p = binary.BigEndian.AppendUint32(p, uint32(len(table)))
		for _, rec := range table {
			p = binary.BigEndian.AppendUint32(p, rec.TrackID)
			p = binary.BigEndian.AppendUint32(p, rec.Seq)
			p = binary.BigEndian.AppendUint64(p, rec.DTS)
			p = binary.BigEndian.AppendUint32(p, rec.Duration)
			b.offsetPos = append(b.offsetPos, len(p))
			p = binary.BigEndian.AppendUint64(p, 0) // patched later
			p = binary.BigEndian.AppendUint32(p, rec.Size)
			p = binary.BigEndian.AppendUint32(p, rec.Flags)
			p = binary.BigEndian.AppendUint32(p, rec.RTPSeq)
		}
	}
	p = binary.BigEndian.AppendUint32(p, 0) // no damage trailer in fixtures
	return p
}

// resolveOffsets writes absolute mdat payload offsets into armd, one
// fragment at a time. chunkPos[ci] is the armf box start of fragment ci.
func (b *builder) resolveOffsets(armd []byte, chunkPos []int) {
	b.lastArmd = armd
	idx := 0
	for ci, table := range b.tables {
		armfLen := len(buildArmf(uint32(ci), len(table)))
		running := chunkPos[ci] + armfLen + 8 // skip armf box and mdat header
		for _, rec := range table {
			pos := b.offsetPos[idx]
			binary.BigEndian.PutUint64(b.lastArmd[pos:pos+8], uint64(running))
			running += len(rec.payload)
			idx++
		}
	}
}
