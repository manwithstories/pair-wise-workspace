// Package remux rewrites fMP4 packaging losslessly: only box structure,
// sequence numbers, decode times and track identifiers are rewritten;
// sample payloads are carried through byte-for-byte.
package remux

import (
	"crypto/sha256"
	"fmt"
	"io"

	mp4 "github.com/abema/go-mp4"
)

// OutSample is one sample to write. Data must be exactly Size bytes.
type OutSample struct {
	Duration uint32
	Size     uint32
	Flags    uint32
	CTO      int32 // composition time offset, preserved from the input
	Data     []byte
}

// OutFrag is one track fragment of an output segment.
type OutFrag struct {
	TrackID uint32
	TFDT    uint64 // (possibly repaired) base media decode time
	Samples []OutSample
}

// memWS is an in-memory io.WriteSeeker used to stage boxes whose size must
// be known before their final contents (trun data offsets) can be computed.
type memWS struct {
	buf []byte
	pos int64
}

func (m *memWS) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if grow := int(end) - len(m.buf); grow > 0 {
		m.buf = append(m.buf, make([]byte, grow)...)
	}
	copy(m.buf[m.pos:end], p)
	m.pos = end
	return len(p), nil
}

func (m *memWS) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = m.pos + offset
	case io.SeekEnd:
		abs = int64(len(m.buf)) + offset
	}
	if abs < 0 {
		return 0, fmt.Errorf("negative seek %d", abs)
	}
	m.pos = abs
	return abs, nil
}

// HashPayload hashes concatenated sample payloads; used for both input-side
// and output-side payload digests so they are directly comparable.
func HashPayload(parts [][]byte) [32]byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func writeLeaf(w *mp4.Writer, box mp4.IImmutableBox) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: box.GetType()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, box, mp4.Context{}); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

// buildMoof marshals styp's following moof box. offsets holds one trun
// data-offset per fragment, relative to the start of the moof box.
func buildMoof(seq uint32, frags []OutFrag, offsets []int32) ([]byte, error) {
	m := &memWS{}
	w := mp4.NewWriter(m)
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoof()}); err != nil {
		return nil, err
	}
	if err := writeLeaf(w, &mp4.Mfhd{SequenceNumber: seq}); err != nil {
		return nil, err
	}
	for i := range frags {
		f := &frags[i]
		if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTraf()}); err != nil {
			return nil, err
		}
		tfhd := &mp4.Tfhd{TrackID: f.TrackID}
		tfhd.SetFlags(mp4.TfhdDefaultBaseIsMoof)
		if err := writeLeaf(w, tfhd); err != nil {
			return nil, err
		}
		tfdt := &mp4.Tfdt{BaseMediaDecodeTimeV1: f.TFDT}
		tfdt.SetVersion(1)
		if err := writeLeaf(w, tfdt); err != nil {
			return nil, err
		}
		trun := &mp4.Trun{SampleCount: uint32(len(f.Samples)), DataOffset: offsets[i]}
		trun.SetFlags(0x001 | 0x100 | 0x200 | 0x400 | 0x800)
		negCTO := false
		for _, s := range f.Samples {
			if s.CTO < 0 {
				negCTO = true
			}
		}
		if negCTO {
			trun.SetVersion(1)
		}
		for _, s := range f.Samples {
			e := mp4.TrunEntry{
				SampleDuration: s.Duration,
				SampleSize:     s.Size,
				SampleFlags:    s.Flags,
			}
			if negCTO {
				e.SampleCompositionTimeOffsetV1 = s.CTO
			} else {
				e.SampleCompositionTimeOffsetV0 = uint32(s.CTO)
			}
			trun.Entries = append(trun.Entries, e)
		}
		if err := writeLeaf(w, trun); err != nil {
			return nil, err
		}
		if _, err := w.EndBox(); err != nil { // traf
			return nil, err
		}
	}
	if _, err := w.EndBox(); err != nil { // moof
		return nil, err
	}
	return m.buf, nil
}

// MarshalSegment writes one output segment (styp, moof, mdat) and returns
// the payload hash of each track. Sample payloads are written verbatim.
func MarshalSegment(w io.WriteSeeker, seq uint32, frags []OutFrag) (map[uint32][32]byte, error) {
	hashes := make(map[uint32][32]byte, len(frags))
	sizes := make([]int64, len(frags))
	for i := range frags {
		var parts [][]byte
		var n int64
		for _, s := range frags[i].Samples {
			if uint32(len(s.Data)) != s.Size {
				return nil, fmt.Errorf("track %d: sample size %d != data %d", frags[i].TrackID, s.Size, len(s.Data))
			}
			parts = append(parts, s.Data)
			n += int64(len(s.Data))
		}
		hashes[frags[i].TrackID] = HashPayload(parts)
		sizes[i] = n
	}
	zero := make([]int32, len(frags))
	moof, err := buildMoof(seq, frags, zero)
	if err != nil {
		return nil, err
	}
	offsets := make([]int32, len(frags))
	base := int64(len(moof)) + 8 // moof + mdat header
	for i := range frags {
		offsets[i] = int32(base)
		base += sizes[i]
	}
	if moof, err = buildMoof(seq, frags, offsets); err != nil {
		return nil, err
	}
	mw := mp4.NewWriter(w)
	styp := &mp4.Styp{MajorBrand: [4]byte{'c', 'm', 'f', 's'}}
	styp.CompatibleBrands = append(styp.CompatibleBrands,
		mp4.CompatibleBrandElem{CompatibleBrand: [4]byte{'c', 'm', 'f', 's'}})
	if err := writeLeaf(mw, styp); err != nil {
		return nil, err
	}
	if _, err := mw.Write(moof); err != nil {
		return nil, err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return nil, err
	}
	for i := range frags {
		for _, s := range frags[i].Samples {
			if _, err := mw.Write(s.Data); err != nil {
				return nil, err
			}
		}
	}
	if _, err := mw.EndBox(); err != nil { // mdat
		return nil, err
	}
	return hashes, nil
}
