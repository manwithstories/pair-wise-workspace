package fixture

import (
	"encoding/binary"

	"github.com/abema/go-mp4"
)

type TrackSpec struct {
	// Kind is "video" or "audio".
	Kind string
	// Timescale is the media timescale (ticks per second).
	Timescale uint32
	// SampleDelta is the duration of every sample in timescale ticks.
	SampleDelta uint32
	// SampleSizes gives each sample's payload size in bytes.
	SampleSizes []uint32
	// SyncSamples lists 1-based sample numbers that are keyframes. When nil
	// and NoSTSS is false, sample 1 is used.
	SyncSamples []int
	// KeyframeMarkers lists 1-based samples whose payload carries the
	// keyframe marker bit. When nil it defaults to SyncSamples, so the sample
	// bytes agree with stss. Setting it to a superset of SyncSamples simulates
	// a lost/incomplete stss index that a sample scan can rebuild.
	KeyframeMarkers []int
	// NoSTSS omits the stss box entirely, meaning "every sample is sync".
	NoSTSS bool
	// OmitSTCO omits stco, producing an incomplete index.
	OmitSTCO bool
	// OmitSTSZ omits stsz, producing a sample table with no sizes.
	OmitSTSZ bool
	// CorruptSizeIdx, when >= 0, inflates that sample's declared size so its
	// byte span falls outside mdat.
	CorruptSizeIdx int
}

// MP4 assembles a complete MP4 file from the given track specs. It returns the
// bytes and a non-nil error only for internal writer failures.
func MP4(tracks []TrackSpec) ([]byte, error) {
	m := &memWS{}
	w := mp4.NewWriter(m)

	ftyp := &mp4.Ftyp{MajorBrand: [4]byte{'i', 's', 'o', 'm'}, MinorVersion: 512}
	ftyp.AddCompatibleBrand([4]byte{'i', 's', 'o', 'm'})
	ftyp.AddCompatibleBrand([4]byte{'m', 'p', '4', '2'})
	if err := writeBox(w, ftyp); err != nil {
		return nil, err
	}
	patches, err := writeMoov(w, m, tracks)
	if err != nil {
		return nil, err
	}
	moovEnd := len(m.buf)

	var payload []byte
	chunkOffsets := make([][]uint32, len(tracks))
	off := uint32(moovEnd + 8)
	for ti, tr := range tracks {
		// Encode keyframe flags in the sample payload so a sample scan can
		// independently reconstruct the index, which is how an incomplete
		// stss is detected and rebuilt. A sample whose first byte has bit 0x80
		// set is a keyframe. Audio carries no keyframe semantics.
		markers := map[int]bool{}
		if tr.Kind != "audio" {
			nums := tr.KeyframeMarkers
			if nums == nil {
				nums = tr.SyncSamples
				if nums == nil && !tr.NoSTSS {
					nums = []int{1}
				}
			}
			for _, n := range nums {
				markers[n] = true
			}
		}
		for si, sz := range tr.SampleSizes {
			chunkOffsets[ti] = append(chunkOffsets[ti], off)
			off += sz
			b := make([]byte, sz)
			for i := range b {
				b[i] = byte(0x40 + ti)
			}
			if markers[si+1] && sz > 0 {
				b[0] |= 0x80
			}
			payload = append(payload, b...)
		}
	}
	for _, p := range patches {
		for i, at := range p.offsets {
			binary.BigEndian.PutUint32(m.buf[at:], chunkOffsets[p.track][i])
		}
	}
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:], uint32(8+len(payload)))
	copy(hdr[4:], "mdat")
	m.buf = append(m.buf, hdr[:]...)
	m.buf = append(m.buf, payload...)
	return append([]byte(nil), m.buf...), nil
}

type stcoPatch struct {
	track   int
	offsets []int
}

func writeMoov(w *mp4.Writer, m *memWS, tracks []TrackSpec) ([]stcoPatch, error) {
	if err := startBox(w, "moov"); err != nil {
		return nil, err
	}
	maxDur := uint32(0)
	for _, tr := range tracks {
		if d := tr.SampleDelta * uint32(len(tr.SampleSizes)); d > maxDur {
			maxDur = d
		}
	}
	if err := writeBox(w, &mp4.Mvhd{Timescale: 1000, DurationV0: maxDur}); err != nil {
		return nil, err
	}
	var patches []stcoPatch
	for ti, tr := range tracks {
		if err := writeTrak(w, m, ti, tr, &patches); err != nil {
			return nil, err
		}
	}
	if err := endBox(w); err != nil {
		return nil, err
	}
	return patches, nil
}

func writeTrak(w *mp4.Writer, m *memWS, ti int, tr TrackSpec, patches *[]stcoPatch) error {
	dur := tr.SampleDelta * uint32(len(tr.SampleSizes))
	for _, step := range []func() error{
		func() error { return startBox(w, "trak") },
		func() error { return writeBox(w, &mp4.Tkhd{TrackID: uint32(ti + 1), DurationV0: dur}) },
		func() error { return startBox(w, "mdia") },
		func() error { return writeBox(w, &mp4.Mdhd{Timescale: tr.Timescale, DurationV0: dur}) },
	} {
		if err := step(); err != nil {
			return err
		}
	}
	handler := [4]byte{'v', 'i', 'd', 'e'}
	if tr.Kind == "audio" {
		handler = [4]byte{'s', 'o', 'u', 'n'}
	}
	if err := writeBox(w, &mp4.Hdlr{HandlerType: handler}); err != nil {
		return err
	}
	if err := startBox(w, "minf"); err != nil {
		return err
	}
	if tr.Kind == "audio" {
		if err := writeBox(w, &mp4.Smhd{}); err != nil {
			return err
		}
	} else if err := writeBox(w, &mp4.Vmhd{}); err != nil {
		return err
	}
	if err := startBox(w, "stbl"); err != nil {
		return err
	}
	if err := writeStsd(w); err != nil {
		return err
	}
	if err := writeBox(w, &mp4.Stts{EntryCount: 1, Entries: []mp4.SttsEntry{{SampleCount: uint32(len(tr.SampleSizes)), SampleDelta: tr.SampleDelta}}}); err != nil {
		return err
	}
	if err := writeBox(w, &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: 1, SampleDescriptionIndex: 1}}}); err != nil {
		return err
	}
	if !tr.OmitSTSZ {
		sizes := make([]uint32, len(tr.SampleSizes))
		copy(sizes, tr.SampleSizes)
		if tr.CorruptSizeIdx >= 0 && tr.CorruptSizeIdx < len(sizes) {
			sizes[tr.CorruptSizeIdx] += 0x00FF_FFFF
		}
		if err := writeBox(w, &mp4.Stsz{SampleCount: uint32(len(sizes)), EntrySize: sizes}); err != nil {
			return err
		}
	}
	if !tr.OmitSTCO {
		if err := startBox(w, "stco"); err != nil {
			return err
		}
		writeRaw(w, []byte{0, 0, 0, 0})
		writeRaw(w, u32(uint32(len(tr.SampleSizes))))
		p := stcoPatch{track: ti}
		for range tr.SampleSizes {
			p.offsets = append(p.offsets, m.pos)
			writeRaw(w, u32(0))
		}
		*patches = append(*patches, p)
		if err := endBox(w); err != nil {
			return err
		}
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
		if err := writeBox(w, &mp4.Stss{EntryCount: uint32(len(sn)), SampleNumber: sn}); err != nil {
			return err
		}
	}
	for i := 0; i < 3; i++ { // stbl, minf, mdia
		if err := endBox(w); err != nil {
			return err
		}
	}
	return endBox(w) // trak
}
