package testdata

import (
	"encoding/binary"

	"github.com/abema/go-mp4"
)

// GenerateMP4 builds one MP4 file from spec. The moov is written after mdat so
// chunk offsets are known; a fault may then be injected by rewriting the file
// with a deliberately damaged field.
func GenerateMP4(spec MP4Spec) []byte {
	m := &memFile{}
	w := mp4.NewWriter(m)

	start(w, mp4.BoxTypeFtyp())
	leaf(w, &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 512,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
			{CompatibleBrand: [4]byte{'i', 's', 'o', '2'}},
			{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}},
		},
	})
	end(w)

	if spec.PaddingBytes > 0 {
		start(w, mp4.BoxTypeFree())
		m.Write(make([]byte, spec.PaddingBytes))
		end(w)
	}

	start(w, mp4.BoxTypeMdat())
	payloadStart := m.off
	for i := 0; i < spec.SampleCount; i++ {
		m.Write(payload(spec.PayloadSeed, i, spec.SampleSize)) //nolint:errcheck
	}
	mdatInfo := end(w)

	writeMoov(w, spec, payloadStart, mdatInfo)

	out := m.buf
	if spec.Fault == FaultTruncatedMoov {
		// Keep ftyp+mdat+moov header, drop the moov payload so its children
		// are truncated exactly where the index would be read.
		if moov := findBox(out, [4]byte{'m', 'o', 'o', 'v'}); moov >= 0 {
			size := int(binary.BigEndian.Uint32(out[moov:]))
			out = out[:moov+size/2]
		}
	}
	return out
}

func writeMoov(w *mp4.Writer, spec MP4Spec, payloadStart int64, mdat *mp4.BoxInfo) {
	start(w, mp4.BoxTypeMoov())
	start(w, mp4.BoxTypeMvhd())
	leaf(w, &mp4.Mvhd{
		Timescale:   1000,
		DurationV0:  uint32(int64(spec.SampleCount) * int64(spec.SampleDelta) * 1000 / int64(spec.Timescale)),
		Rate:        0x10000,
		Volume:      0x100,
		NextTrackID: 2,
	})
	end(w)
	start(w, mp4.BoxTypeTrak())
	start(w, mp4.BoxTypeTkhd())
	leaf(w, &mp4.Tkhd{TrackID: 1, DurationV0: 1000, Width: 64 << 16, Height: 48 << 16})
	end(w)
	start(w, mp4.BoxTypeMdia())
	start(w, mp4.BoxTypeMdhd())
	leaf(w, &mp4.Mdhd{Timescale: spec.Timescale,
		DurationV0: uint32(int64(spec.SampleCount) * int64(spec.SampleDelta))})
	end(w)
	start(w, mp4.BoxTypeHdlr())
	leaf(w, &mp4.Hdlr{HandlerType: [4]byte{'v', 'i', 'd', 'e'}, Name: "VideoHandler"})
	end(w)
	start(w, mp4.BoxTypeMinf())
	start(w, mp4.BoxTypeStbl())
	writeStts(w, spec)
	writeStss(w, spec)
	writeStsz(w, spec)
	writeStsc(w, spec)
	writeStco(w, spec, payloadStart, mdat)
	end(w) // stbl
	end(w) // minf
	end(w) // mdia
	end(w) // trak
	end(w) // moov
}

func writeStts(w *mp4.Writer, spec MP4Spec) {
	delta := spec.SampleDelta
	if spec.Fault == FaultZeroDelta {
		delta = 0
	}
	stts := &mp4.Stts{Entries: []mp4.SttsEntry{{SampleCount: uint32(spec.SampleCount), SampleDelta: delta}}}
	stts.EntryCount = 1
	start(w, mp4.BoxTypeStts())
	leaf(w, stts)
	end(w)
}

func writeStss(w *mp4.Writer, spec MP4Spec) {
	if spec.Fault == FaultNoStss {
		return
	}
	var nums []uint32
	interval := spec.KeyInterval
	if interval <= 0 {
		interval = spec.SampleCount
	}
	for i := 0; i < spec.SampleCount; i += interval {
		nums = append(nums, uint32(i+1))
	}
	if spec.Fault == FaultStssOutOfRange {
		nums = append(nums, uint32(spec.SampleCount+10))
	}
	stss := &mp4.Stss{SampleNumber: nums}
	stss.EntryCount = uint32(len(nums))
	start(w, mp4.BoxTypeStss())
	leaf(w, stss)
	end(w)
}

func writeStsz(w *mp4.Writer, spec MP4Spec) {
	stsz := &mp4.Stsz{SampleSize: uint32(spec.SampleSize), SampleCount: uint32(spec.SampleCount)}
	start(w, mp4.BoxTypeStsz())
	leaf(w, stsz)
	end(w)
}

func writeStsc(w *mp4.Writer, spec MP4Spec) {
	stsc := &mp4.Stsc{Entries: []mp4.StscEntry{
		{FirstChunk: 1, SamplesPerChunk: uint32(spec.SampleCount), SampleDescriptionIndex: 1},
	}}
	stsc.EntryCount = 1
	start(w, mp4.BoxTypeStsc())
	leaf(w, stsc)
	end(w)
}

func writeStco(w *mp4.Writer, spec MP4Spec, payloadStart int64, mdat *mp4.BoxInfo) {
	offset := uint32(payloadStart)
	if spec.Fault == FaultChunkOutOfFile {
		offset = uint32(mdat.Offset + mdat.Size + 4096)
	}
	stco := &mp4.Stco{ChunkOffset: []uint32{offset}}
	stco.EntryCount = 1
	start(w, mp4.BoxTypeStco())
	leaf(w, stco)
	end(w)
}
