package stream

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"testing"

	"github.com/abema/go-mp4"
)

// memWS is a minimal in-memory io.WriteSeeker that supports overwriting
// already-written regions. It lets the fixture writer emit moov first, then
// patch stco chunk offsets once mdat placement is known.
type memWS struct {
	buf []byte
	pos int
}

func (m *memWS) Write(p []byte) (int, error) {
	end := m.pos + len(p)
	if end > len(m.buf) {
		nb := make([]byte, end)
		copy(nb, m.buf)
		m.buf = nb
	}
	copy(m.buf[m.pos:], p)
	m.pos = end
	return len(p), nil
}

func (m *memWS) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = int64(m.pos) + offset
	case io.SeekEnd:
		abs = int64(len(m.buf)) + offset
	default:
		return 0, fmt.Errorf("bad whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("negative seek")
	}
	m.pos = int(abs)
	return abs, nil
}

type mediaTrackSpec struct {
	Kind        string // "video" or "audio"
	Timescale   uint32
	SampleDelta uint32
	SampleSizes []uint32
	SyncSamples []int // 1-based; nil and !NoSTSS -> {1}
	NoSTSS      bool
	OmitSTCO    bool
	OmitSTSZ    bool
	// CorruptSizeIdx, when >=0, inflates that sample's declared size so its
	// byte span falls outside every mdat (a corrupt sample-length fixture).
	CorruptSizeIdx int
	// OmitTrakFractional leaves mdhd duration as-is; reserved for later use.
}

func buildMP4(t *testing.T, tracks []mediaTrackSpec) []byte {
	t.Helper()
	m := &memWS{}
	w := mp4.NewWriter(m)

	ftyp := &mp4.Ftyp{MajorBrand: [4]byte{'i', 's', 'o', 'm'}, MinorVersion: 512}
	ftyp.AddCompatibleBrand([4]byte{'i', 's', 'o', 'm'})
	ftyp.AddCompatibleBrand([4]byte{'m', 'p', '4', '2'})
	writeBox(t, w, ftyp)

	preMoov := len(m.buf)
	patches := writeMoov(t, w, m, tracks)
	moovEnd := len(m.buf)
	_ = preMoov

	// Compute payload from mdat and chunk offsets (one sample per chunk).
	var payload []byte
	chunkOffsets := make([][]uint32, len(tracks))
	off := uint32(moovEnd + 8)
	for ti, tr := range tracks {
		for _, sz := range tr.SampleSizes {
			chunkOffsets[ti] = append(chunkOffsets[ti], off)
			off += sz
			b := make([]byte, sz)
			for i := range b {
				b[i] = byte(0x40 + ti)
			}
			payload = append(payload, b...)
		}
	}
	for _, p := range patches {
		for i, at := range p.offsets {
			binary.BigEndian.PutUint32(m.buf[at:], chunkOffsets[p.track][i])
		}
	}
	// Append mdat header + payload.
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:], uint32(8+len(payload)))
	copy(hdr[4:], "mdat")
	m.buf = append(m.buf, hdr[:]...)
	m.buf = append(m.buf, payload...)
	return append([]byte(nil), m.buf...)
}

type stcoPatch struct {
	track   int
	offsets []int
}

func writeMoov(t *testing.T, w *mp4.Writer, m *memWS, tracks []mediaTrackSpec) []stcoPatch {
	t.Helper()
	startBox(t, w, "moov")
	maxDur := uint32(0)
	for _, tr := range tracks {
		if d := tr.SampleDelta * uint32(len(tr.SampleSizes)); d > maxDur {
			maxDur = d
		}
	}
	writeBox(t, w, &mp4.Mvhd{Timescale: 1000, DurationV0: maxDur})
	var patches []stcoPatch
	for ti, tr := range tracks {
		startBox(t, w, "trak")
		writeBox(t, w, &mp4.Tkhd{TrackID: uint32(ti + 1), DurationV0: tr.SampleDelta * uint32(len(tr.SampleSizes))})
		startBox(t, w, "mdia")
		writeBox(t, w, &mp4.Mdhd{Timescale: tr.Timescale, DurationV0: tr.SampleDelta * uint32(len(tr.SampleSizes))})
		handler := [4]byte{'v', 'i', 'd', 'e'}
		if tr.Kind == "audio" {
			handler = [4]byte{'s', 'o', 'u', 'n'}
		}
		writeBox(t, w, &mp4.Hdlr{HandlerType: handler})
		startBox(t, w, "minf")
		if tr.Kind == "audio" {
			writeBox(t, w, &mp4.Smhd{})
		} else {
			writeBox(t, w, &mp4.Vmhd{})
		}
		startBox(t, w, "stbl")

		// stsd: one avc1 VisualSampleEntry (78-byte fixed body, no children).
		startBox(t, w, "stsd")
		writeRaw(t, w, []byte{0, 0, 0, 0}) // FullBox v/f
		writeRaw(t, w, u32(1))
		startBox(t, w, "avc1")
		entry := make([]byte, 78)
		binary.BigEndian.PutUint16(entry[6:], 1) // data_reference_index
		binary.BigEndian.PutUint16(entry[24:], 1920)
		binary.BigEndian.PutUint16(entry[26:], 1080)
		writeRaw(t, w, entry)
		endBox(t, w)
		endBox(t, w) // stsd

		writeBox(t, w, &mp4.Stts{EntryCount: 1, Entries: []mp4.SttsEntry{{SampleCount: uint32(len(tr.SampleSizes)), SampleDelta: tr.SampleDelta}}})
		writeBox(t, w, &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: 1, SampleDescriptionIndex: 1}}})
		if !tr.OmitSTSZ {
			sizes := make([]uint32, len(tr.SampleSizes))
			copy(sizes, tr.SampleSizes)
			if tr.CorruptSizeIdx >= 0 && tr.CorruptSizeIdx < len(sizes) {
				sizes[tr.CorruptSizeIdx] += 0x00FF_FFFF
			}
			writeBox(t, w, &mp4.Stsz{SampleCount: uint32(len(sizes)), EntrySize: sizes})
		}
		if !tr.OmitSTCO {
			startBox(t, w, "stco")
			writeRaw(t, w, []byte{0, 0, 0, 0})
			writeRaw(t, w, u32(uint32(len(tr.SampleSizes))))
			p := stcoPatch{track: ti}
			for range tr.SampleSizes {
				p.offsets = append(p.offsets, m.pos)
				writeRaw(t, w, u32(0))
			}
			patches = append(patches, p)
			endBox(t, w)
		}
		if !tr.NoSTSS {
			nums := tr.SyncSamples
			if nums == nil {
				nums = []int{1}
			}
			sn := make([]uint32, len(nums))
			for i, n := range nums {
				sn[i] = uint32(n)
			}
			writeBox(t, w, &mp4.Stss{EntryCount: uint32(len(sn)), SampleNumber: sn})
		}
		endBox(t, w) // stbl
		endBox(t, w) // minf
		endBox(t, w) // mdia
		endBox(t, w) // trak
	}
	endBox(t, w) // moov
	return patches
}

func startBox(t *testing.T, w *mp4.Writer, typ string) {
	t.Helper()
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.StrToBoxType(typ), Size: 8, HeaderSize: 8}); err != nil {
		t.Fatalf("start %s: %v", typ, err)
	}
}

func endBox(t *testing.T, w *mp4.Writer) {
	t.Helper()
	if _, err := w.EndBox(); err != nil {
		t.Fatalf("end box: %v", err)
	}
}

func writeBox(t *testing.T, w *mp4.Writer, box mp4.IBox) {
	t.Helper()
	payload := &bytes.Buffer{}
	if _, err := mp4.Marshal(payload, box, mp4.Context{}); err != nil {
		t.Fatalf("marshal %s: %v", box.GetType(), err)
	}
	startBox(t, w, box.GetType().String())
	writeRaw(t, w, payload.Bytes())
	endBox(t, w)
}

func writeRaw(t *testing.T, w *mp4.Writer, b []byte) {
	t.Helper()
	if _, err := w.Write(b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func u32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}
