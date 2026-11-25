package fixture

import (
	"encoding/binary"

	"github.com/abema/go-mp4"

	"mediad/internal/media"
	"mediad/internal/mp4util"
)

// buildInitSegment produces a CMAF-style init segment with ftyp and moov for
// one camera. The stsd sample entry is derived from the camera codec so the
// adapter's codec mapping and byte-preservation can be verified.
func buildInitSegment(cam Camera, fragDur int64) []byte {
	w := mp4util.New()
	_ = w.WriteFtyp("iso6", "iso6", "cmfc", "isom")

	w.Start("moov")
	mvhd := &mp4.Mvhd{
		Timescale:   90000,
		DurationV0:  uint32(fragDur * 180),
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      identityMatrix(),
		NextTrackID: 2,
	}
	_ = w.Marshal(mvhd)
	writeTrak(w, cam)
	w.Start("mvex")
	_ = w.Marshal(&mp4.Trex{TrackID: 1, DefaultSampleDescriptionIndex: 1})
	_ = w.End()
	_ = w.End()
	return w.Bytes()
}

func identityMatrix() [9]int32 {
	return [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
}

func writeTrak(w *mp4util.BoxWriter, cam Camera) {
	w.Start("trak")
	tkhd := &mp4.Tkhd{
		TrackID:    1,
		DurationV0: 0,
		Volume:     0,
		Matrix:     identityMatrix(),
		Width:      uint32(cam.Width) << 16,
		Height:     uint32(cam.Height) << 16,
	}
	tkhd.SetFlags(0x000003) // enabled | in movie
	_ = w.Marshal(tkhd)

	w.Start("mdia")
	mdhd := &mp4.Mdhd{Timescale: uint32(cam.Timescale), DurationV0: 0}
	mdhd.Language = [3]byte{'u', 'n', 'd'}
	_ = w.Marshal(mdhd)

	h := &mp4.Hdlr{}
	h.HandlerType = [4]byte{'v', 'i', 'd', 'e'}
	_ = w.Marshal(h)

	w.Start("minf")
	if err := w.WriteFullBox("vmhd", 0, 1, make([]byte, 8)); err != nil {
		panic(err)
	}
	w.Start("dinf")
	w.Start("dref")
	// dref: version/flags, entry_count=1, and a self-contained url box.
	urlBox := []byte{0x00, 0x00, 0x00, 0x0C, 'u', 'r', 'l', ' ', 0x00, 0x00, 0x00, 0x01}
	if err := w.WriteFullBox("dref", 0, 0, append([]byte{0, 0, 0, 1}, urlBox...)); err != nil {
		panic(err)
	}
	_ = w.End()
	_ = w.End()

	w.Start("stbl")
	if err := w.WriteStsd([][]byte{sampleEntry(cam)}); err != nil {
		panic(err)
	}
	writeEmptyStblTables(w)
	_ = w.End() // stbl
	_ = w.End() // minf
	_ = w.End() // mdia
	_ = w.End() // trak
}

// sampleEntry builds an avc1/hvc1 visual sample entry with a config box.
// The stsd child is preserved verbatim through remux.
func sampleEntry(cam Camera) []byte {
	entryType := "avc1"
	if cam.Codec == "hvc1" {
		entryType = "hvc1"
	}
	ew := mp4util.New()
	ew.Start(entryType)
	body := make([]byte, 78)
	binary.BigEndian.PutUint16(body[6:], 1) // data_reference_index
	binary.BigEndian.PutUint16(body[24:], uint16(cam.Width))
	binary.BigEndian.PutUint16(body[26:], uint16(cam.Height))
	binary.BigEndian.PutUint32(body[28:], 0x00480000)
	binary.BigEndian.PutUint32(body[32:], 0x00480000)
	binary.BigEndian.PutUint16(body[40:], 1) // frame_count
	binary.BigEndian.PutUint16(body[74:], 0x0018)
	binary.BigEndian.PutUint16(body[76:], 0xFFFF)
	ew.WriteRawBytes(body)
	if entryType == "hvc1" {
		hvcc := make([]byte, 23)
		hvcc[0] = 1
		_ = ew.WriteRaw("hvcC", hvcc)
	} else {
		avcc := []byte{0x01, 0x64, 0x00, 0x28, 0xFF, 0xE1, 0x00, 0x04, 0x67, 0x64, 0x00, 0x28, 0x01, 0x00, 0x04, 0x68, 0xEE, 0x3C, 0x80}
		_ = ew.WriteRaw("avcC", avcc)
	}
	_ = ew.End()
	return ew.Bytes()
}

// writeEmptyStblTables emits the minimal empty sample tables CMAF requires.
func writeEmptyStblTables(w *mp4util.BoxWriter) {
	_ = w.WriteFullBox("stts", 0, 0, make([]byte, 4))
	_ = w.WriteFullBox("stsc", 0, 0, make([]byte, 4))
	_ = w.WriteFullBox("stsz", 0, 0, make([]byte, 8))
	_ = w.WriteFullBox("stco", 0, 0, make([]byte, 4))
}

// buildFragmentedSegment assembles styp + moof + mdat for one camera segment.
// Sample payload bytes are copied verbatim into mdat. PTS/DTS equal the decode
// time when interleaving is disabled by the fixture profile.
func buildFragmentedSegment(cam Camera, index int, startTicks int64, durs []int64, payloads [][]byte) []byte {
	w := mp4util.New()
	_ = w.WriteStyp("msdh", "msdh", "msix")

	sizes := make([]uint32, len(payloads))
	for i, p := range payloads {
		sizes[i] = uint32(len(p))
	}

	moofPayload := func(dataOffset int32) []byte {
		m := mp4util.New()
		m.Start("moof")
		_ = m.WriteFullBox("mfhd", 0, 0, u32(index+1))
		m.Start("traf")
		tfhd := &mp4.Tfhd{TrackID: 1}
		tfhd.SetFlags(0x020000) // default-base-is-moof
		_ = m.Marshal(tfhd)

		tfdt := &mp4.Tfdt{}
		tfdt.SetVersion(1)
		tfdt.BaseMediaDecodeTimeV1 = uint64(startTicks)
		_ = m.Marshal(tfdt)

		trun := &mp4.Trun{SampleCount: uint32(len(payloads)), DataOffset: dataOffset}
		trun.SetFlags(0x000001 | 0x000100 | 0x000200)
		trun.Entries = make([]mp4.TrunEntry, len(payloads))
		for i := range payloads {
			trun.Entries[i] = mp4.TrunEntry{
				SampleDuration: uint32(durs[i]),
				SampleSize:     sizes[i],
			}
		}
		_ = m.Marshal(trun)
		_ = m.End() // traf
		_ = m.End() // moof
		return m.Bytes()
	}

	// The moof length is independent of the DataOffset value, so one pass with
	// a dummy offset fixes the layout for the real pass.
	dummy := moofPayload(0)
	dataOffset := int32(len(dummy) + 8) // moof bytes + mdat header
	moof := moofPayload(dataOffset)
	if len(moof) != len(dummy) {
		panic("fixture: moof size changed between passes")
	}

	w.WriteRawBytes(moof)
	mdat := mp4util.New()
	mdat.Start("mdat")
	for _, p := range payloads {
		mdat.WriteRawBytes(p)
	}
	_ = mdat.End()
	w.WriteRawBytes(mdat.Bytes())
	return w.Bytes()
}

func u32(v int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(v))
	return b
}

// truncateMoof corrupts a segment by cutting bytes from the end so the last
// sample's declared extent runs past the available mdat bytes.
func truncateMoof(seg []byte, bytesToCut int64) []byte {
	if bytesToCut <= 0 {
		bytesToCut = 17
	}
	n := int64(len(seg)) - bytesToCut
	if n < 16 || n >= int64(len(seg)) {
		n = int64(len(seg)) * 2 / 3
	}
	return append([]byte(nil), seg[:n]...)
}

// wrapEnc expresses a tick value modulo 2^33 for the 33-bit PTS fixture.
func wrapEnc(v int64) uint64 {
	return uint64(v % media.Wrap33)
}
