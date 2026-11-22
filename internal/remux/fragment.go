// Package remux: fragment assembly (moof + mdat).
package remux

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"archive/internal/demux"
	"archive/internal/fault"
)

// fragment writes one moof whose truns reference a following mdat.
// Each traf's data offset is relative to the moof start (tfhd carries
// default-base-is-moof), so the layout stays self-describing.
func fragment(inputs []Input) (moof, mdat []byte, err error) {
	tracks := make([]demux.Track, len(inputs))
	ds := make([][]int64, len(inputs))
	for i, in := range inputs {
		tracks[i], ds[i] = in.Track, in.DTS
	}
	var payload bytes.Buffer
	// First pass: assign each track a contiguous byte range. A single
	// mdat uses a 32-bit size, so the payload must stay below 4 GiB;
	// the 64 MiB per-segment cap makes this a defensive check.
	if total := totalBytes(tracks); total > (1<<32)-8 {
		return nil, nil, fault.Newf(fault.Fatal, "remux", "", "segment too large for one mdat")
	}
	type span struct{ off, size int }
	spans := make([]span, len(tracks))
	for i, tr := range tracks {
		spans[i].off = payload.Len()
		for _, s := range tr.Samples {
			payload.Write(s.Data)
		}
		spans[i].size = payload.Len() - spans[i].off
	}
	mdat = box("mdat", payload.Bytes())

	var mfhd bytes.Buffer
	u32(&mfhd, 1)
	mfhdBox := fullBox("mfhd", 0, 0, mfhd.Bytes())

	var trafs [][]byte
	for i, tr := range tracks {
		if len(tr.Samples) == 0 {
			continue
		}
		var tfhd bytes.Buffer
		u32(&tfhd, uint32(i+1))
		tfhdBox := fullBox("tfhd", 0, 0x020000, tfhd.Bytes()) // default-base-is-moof

		var tfdt bytes.Buffer
		base := uint64(ds[i][0])
		u64(&tfdt, base)
		tfdtBox := fullBox("tfdt", 1, 0, tfdt.Bytes())

		// Data offset = moof size + mdat header + track offset. The moof
		// size is only known after its truns are written, so estimate
		// then patch: trun layout is fixed once entries are known.
		var trunBody bytes.Buffer
		u32(&trunBody, uint32(len(tr.Samples)))
		var dataOff int32
		u32(&trunBody, uint32(dataOff)) // placeholder
		for _, s := range tr.Samples {
			u32(&trunBody, s.Dur)
			u32(&trunBody, uint32(len(s.Data)))
			u32(&trunBody, uint32(int32(s.CTSOff)))
		}
		// Flags: data-offset | duration | size | composition offset (v1).
		trunBox := fullBox("trun", 1, 0x000001|0x000100|0x000200|0x000800, trunBody.Bytes())
		trafs = append(trafs, container("traf", tfhdBox, tfdtBox, trunBox))
	}
	moof = container("moof", append([][]byte{mfhdBox}, trafs...)...)

	// Now patch each trun's data_offset in place. Locate truns by
	// scanning the moof recursively for the "trun" box and matching
	// order against the non-empty tracks.
	offs := findBoxes(moof, "trun")
	mi := 0
	for i, tr := range tracks {
		if len(tr.Samples) == 0 {
			continue
		}
		if mi >= len(offs) {
			return nil, nil, fault.Newf(fault.Fatal, "remux", "", "internal: trun index overflow")
		}
		off := offs[mi]
		// fullbox version+flags (4) + sample_count (4) + data_offset(4)
		pos := off + 8 + 4 + 4
		val := int32(len(moof) + 8 + spans[i].off)
		binary.BigEndian.PutUint32(moof[pos:], uint32(val))
		mi++
	}
	return moof, mdat, nil
}

func totalBytes(tracks []demux.Track) int64 {
	var n int64
	for _, tr := range tracks {
		for _, s := range tr.Samples {
			n += int64(len(s.Data))
		}
	}
	return n
}

// findBoxes returns the offsets of every box of type typ within buf,
// descending into known container boxes.
func findBoxes(buf []byte, typ string) []int {
	var out []int
	var walk func(start, end int)
	walk = func(start, end int) {
		for p := start; p+8 <= end; {
			size := int(binary.BigEndian.Uint32(buf[p:]))
			t := string(buf[p+4 : p+8])
			if size < 8 || p+size > end {
				return
			}
			if t == typ {
				out = append(out, p)
			}
			if isContainer(t) {
				walk(p+8, p+size)
			}
			p += size
		}
	}
	walk(0, len(buf))
	return out
}

func isContainer(t string) bool {
	switch t {
	case "moov", "trak", "mdia", "minf", "stbl", "moof", "traf", "mvex", "dinf":
		return true
	}
	return false
}

func writeMatrix(b *bytes.Buffer) {
	for _, v := range []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000} {
		u32(b, v)
	}
}

func packLang(l string) uint16 {
	b := []byte(l)
	if len(b) != 3 {
		b = []byte("und")
	}
	var v uint16
	for _, c := range b {
		v = v<<5 | uint16(c-0x60)&0x1f
	}
	return v
}

func u16(b *bytes.Buffer, v uint16) {
	var x [2]byte
	binary.BigEndian.PutUint16(x[:], v)
	b.Write(x[:])
}

func u32(b *bytes.Buffer, v uint32) {
	var x [4]byte
	binary.BigEndian.PutUint32(x[:], v)
	b.Write(x[:])
}

func u64(b *bytes.Buffer, v uint64) {
	var x [8]byte
	binary.BigEndian.PutUint64(x[:], v)
	b.Write(x[:])
}

// String renders the track table for terminal output.
func (i Info) String() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "tracks=%d", len(i.Tracks))
	for _, t := range i.Tracks {
		fmt.Fprintf(&b, " [%d %s %s %s %d samples %.3f..%.3fs]",
			t.ID, t.Role, t.Lang, t.Codec, t.Samples, t.Start.Float64(), t.End.Float64())
	}
	return b.String()
}
