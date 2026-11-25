package fixtgen

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
)

// WriteFragmentedSource renders a fragmented MP4: styp + three self-contained
// moof/mdat fragments. For the incomplete_frag segment the last trun entry's
// payload bytes are removed while trun still declares them, which the parser
// must detect without inventing a keyframe.
func WriteFragmentedSource(dir string, seg Segment) (string, error) {
	buf := &bytes.Buffer{}
	writeStyp(buf)

	for frag := 1; frag <= 3; frag++ {
		var samples [][]byte
		for s := 0; s < 4; s++ {
			idx := (frag-1)*4 + s
			samples = append(samples, makeSample(seg.Index, 0, idx, "avc1")[4:])
		}
		// Fragment 3 of the faulty segment loses its final sample bytes.
		incomplete := seg.Fault == FaultIncompleteFrag && frag == 3
		writeFragment(buf, uint32(frag), samples, incomplete)
	}

	path := filepath.Join(dir, seg.ID+".mp4")
	return path, os.WriteFile(path, buf.Bytes(), 0o644)
}

func writeStyp(buf *bytes.Buffer) {
	payload := append([]byte("msdh"), u32b(0)...)
	payload = append(payload, []byte("msdh")...)
	payload = append(payload, []byte("iso8")...)
	b("styp").raw(payload).write(buf)
}

func writeFragment(buf *bytes.Buffer, seq uint32, samples [][]byte, incomplete bool) {
	// trun flags: data-offset(0x01) + sample-size(0x200) + sample-flags(0x400).
	const trunFlags = 0x000601
	trun := buildTrun(samples, trunFlags, incomplete)
	tfhd := buildTfhd(1)
	tfdt := buildTfdt(uint64(seq-1) * 4 * 3000)
	moof := b("moof").
		add(*b("mfhd").raw(append(full(0, 0), u32b(seq)...))).
		add(*b("traf").add(tfhd).add(tfdt).add(trun))

	// data_offset per ISO 14496-12 is relative to the start of the moof box:
	// skip the whole moof plus the 8-byte mdat header to reach the first byte.
	var moofBuf bytes.Buffer
	moof.write(&moofBuf)
	dataOffset := moofBuf.Len() + 8

	mdatPayload := bytes.NewBuffer(nil)
	for i, s := range samples {
		if incomplete && i == len(samples)-1 {
			continue // declared in trun but bytes absent
		}
		mdatPayload.Write(s)
	}
	// Patch trun data-offset now that moof size is known.
	fixTrunOffset(&moofBuf, moof.len(), dataOffset)

	buf.Write(moofBuf.Bytes())
	mh := make([]byte, 8)
	binary.BigEndian.PutUint32(mh, uint32(8+mdatPayload.Len()))
	copy(mh[4:], "mdat")
	buf.Write(mh)
	buf.Write(mdatPayload.Bytes())
}

func buildTrun(samples [][]byte, flags uint32, incomplete bool) box {
	n := len(samples)
	body := make([]byte, 8) // version/flags + count
	copy(body[0:4], full(0, flags))
	binary.BigEndian.PutUint32(body[4:8], uint32(n))
	body = append(body, u32b(0)...) // data offset placeholder
	for i, s := range samples {
		body = append(body, u32b(uint32(len(s)))...)
		// sample flags: 0 for keyframe, 0x20000 for non-sync (depends_on=2).
		var sampleFlags uint32
		if i%KeyEvery != 0 {
			sampleFlags = 0x00020000
		}
		body = append(body, u32b(sampleFlags)...)
	}
	return *b("trun").raw(body)
}

func buildTfhd(trackID uint32) box {
	body := make([]byte, 8)
	copy(body[0:4], full(0, 0x020000)) // default-base-is-moof
	binary.BigEndian.PutUint32(body[4:8], trackID)
	return *b("tfhd").raw(body)
}

func buildTfdt(base uint64) box {
	body := make([]byte, 12)
	body[0] = 1 // version 1, 64-bit baseMediaDecodeTime
	binary.BigEndian.PutUint64(body[4:12], base)
	return *b("tfdt").raw(body)
}

// fixTrunOffset rewrites the trun data-offset field. The placeholder sits at
// moofLen (known) minus the trailing trun; we locate it by scanning for "trun".
func fixTrunOffset(moof *bytes.Buffer, moofLen, offset int) {
	raw := moof.Bytes()
	idx := bytes.Index(raw, []byte("trun"))
	if idx < 0 {
		return
	}
	// trun: header at idx-4; payload starts idx+4; after ver/flags(4)+count(4)
	// sits the data-offset field.
	pos := idx + 4 + 8
	binary.BigEndian.PutUint32(raw[pos:], uint32(offset))
}
