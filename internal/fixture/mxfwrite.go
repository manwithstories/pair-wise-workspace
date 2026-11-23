package fixture

import (
	"bytes"
	"encoding/binary"

	"mediaroute/internal/probe"
)

// BuildMXF renders the local MXF-flavoured KLV container for the spec.
// Layout: magic, header partition, preface (container colour), one track
// with its colour/crop/codec items, index table, essence, footer.
func (s Spec) BuildMXF() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(probe.MXFMagic)
	klv := func(key [16]byte, val []byte) {
		hdr := make([]byte, 20)
		copy(hdr[0:16], key[:])
		binary.BigEndian.PutUint32(hdr[16:], uint32(len(val)))
		b.Write(hdr)
		b.Write(val)
	}
	klv(probe.ULHeaderPartition, []byte{0})
	if s.ContainerColour != nil {
		klv(probe.ULPreface, colourWire(*s.ContainerColour))
	}
	klv(probe.ULMaterialPackage, []byte{0})
	klv(probe.ULSourcePackage, []byte{0})

	th := make([]byte, 32)
	scale := s.Timescale
	if s.BadTimeBase {
		scale = 0
	}
	binary.BigEndian.PutUint32(th[0:], 1) // track id
	binary.BigEndian.PutUint32(th[4:], 0) // video
	binary.BigEndian.PutUint32(th[8:], scale)
	binary.BigEndian.PutUint32(th[12:], 1)
	binary.BigEndian.PutUint32(th[16:], uint32(s.SampleCount))
	binary.BigEndian.PutUint32(th[20:], s.Width)
	binary.BigEndian.PutUint32(th[24:], s.Height)
	binary.BigEndian.PutUint32(th[28:], uint32(s.Rotation))
	klv(probe.ULTrack, th)
	if s.TrackColour != nil {
		klv(probe.ULColor, colourWire(*s.TrackColour))
	}
	if s.IllegalCrop {
		crop := make([]byte, 16)
		binary.BigEndian.PutUint32(crop[8:], s.Width+200)
		binary.BigEndian.PutUint32(crop[12:], s.Height)
		klv(probe.ULCrop, crop)
	}
	codec := s.Codec
	if codec == "" {
		codec = "avc1"
	}
	klv(probe.ULCodec, []byte(codec))

	// Index table and essence are emitted together so offsets are exact.
	var idx bytes.Buffer
	var essence bytes.Buffer
	for i := 0; i < s.SampleCount; i++ {
		size := SampleSize(s, i)
		buf := make([]byte, size)
		SampleData(s, i, buf)
		off := uint64(0) // absolute offset patched after final assembly
		row := make([]byte, 32)
		binary.BigEndian.PutUint32(row[0:], uint32(i))
		flags := uint32(0)
		if s.IsKeyframe(i) {
			flags |= probe.MXFKeyframe
		}
		binary.BigEndian.PutUint32(row[4:], flags)
		binary.BigEndian.PutUint32(row[8:], s.FrameDelta)
		binary.BigEndian.PutUint64(row[16:], off)
		binary.BigEndian.PutUint64(row[24:], uint64(size))
		idx.Write(row)
		essence.Write(buf)
	}
	klv(probe.ULIndexTable, idx.Bytes())
	essenceBytes := essence.Bytes()
	klv(probe.ULEssence, essenceBytes)
	klv(probe.ULFooter, []byte{0})

	out := b.Bytes()
	patchMXFOffsets(out, essenceBytes)
	if s.TruncateMDAT {
		out = out[:len(out)-len(essenceBytes)/4]
	}
	return out, nil
}

func colourWire(c [3]uint16) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint16(buf[0:], c[0])
	binary.BigEndian.PutUint16(buf[2:], c[1])
	binary.BigEndian.PutUint16(buf[4:], c[2])
	return buf
}

// patchMXFOffsets rewrites index rows with the absolute file offset of
// every essence sample, located via the KLV item boundaries.
func patchMXFOffsets(raw []byte, essence []byte) {
	pos := len(probe.MXFMagic)
	var essenceStart, indexStart, indexLen int
	for pos+20 <= len(raw) {
		key := [16]byte(raw[pos : pos+16])
		length := int(binary.BigEndian.Uint32(raw[pos+16 : pos+20]))
		valStart := pos + 20
		switch key {
		case probe.ULIndexTable:
			indexStart, indexLen = valStart, length
		case probe.ULEssence:
			essenceStart = valStart
		}
		pos = valStart + length
	}
	if indexStart == 0 || essenceStart == 0 {
		return
	}
	cursor := essenceStart
	rows := indexLen / 32
	for i := 0; i < rows; i++ {
		row := indexStart + i*32
		size := binary.BigEndian.Uint64(raw[row+24:])
		binary.BigEndian.PutUint64(raw[row+16:], uint64(cursor))
		cursor += int(size)
	}
	_ = essence
}
