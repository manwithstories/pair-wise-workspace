package ingest

import (
	"encoding/binary"

	mp4 "github.com/abema/go-mp4"
)

func writeVideoMinf(w *mp4.Writer, spec FixtureSpec) {
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()})

	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeVmhd()})
	_, _ = mp4.Marshal(w, &mp4.Vmhd{FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 1}}}, mp4.Context{})
	_, _ = w.EndBox()

	writeDinf(w)

	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()})
	writeVideoStsd(w, spec)
	writeStts(w, spec.VideoSamples, spec.VideoTimescale/30)
	writeStsc(w, spec.VideoSamples)
	writeStsz(w, spec.VideoSamples, 64)
	writeStco(w, 0)
	writeStss(w, spec)
	_, _ = w.EndBox() // stbl
	_, _ = w.EndBox() // minf
}

func writeAudioMinf(w *mp4.Writer, spec FixtureSpec, n uint32) {
	rate, ch := spec.AudioSampleRate, spec.AudioChannels
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()})
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeSmhd()})
	_, _ = mp4.Marshal(w, &mp4.Smhd{}, mp4.Context{})
	_, _ = w.EndBox()
	writeDinf(w)

	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()})
	writeAudioStsd(w, spec)
	// one sample per video frame duration (rate/30 frames of audio)
	writeStts(w, n, rate/30)
	writeStsc(w, n)
	writeStsz(w, n, uint32(ch)*4)
	writeStco(w, 0)
	_, _ = w.EndBox()
	_, _ = w.EndBox()
}

func writeDinf(w *mp4.Writer) {
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDinf()})
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDref()})
	_, _ = mp4.Marshal(w, &mp4.Dref{EntryCount: 1}, mp4.Context{})
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeUrl()})
	_, _ = mp4.Marshal(w, &mp4.Url{FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 1}}}, mp4.Context{}) // self-contained
	_, _ = w.EndBox()
	_, _ = w.EndBox() // dref
	_, _ = w.EndBox() // dinf
}

func writeVideoStsd(w *mp4.Writer, spec FixtureSpec) {
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsd()})
	_, _ = mp4.Marshal(w, &mp4.Stsd{EntryCount: 1}, mp4.Context{})

	// avc1 custom box with avcC child
	_, _ = w.StartBox(&mp4.BoxInfo{Type: boxType("avc1")})
	_, _ = mp4.Marshal(w, newFixed("avc1", 78, avc1Payload(spec)), mp4.Context{})

	_, _ = w.StartBox(&mp4.BoxInfo{Type: boxType("avcC")})
	_, _ = mp4.Marshal(w, &RawBox{AnyTypeBox: mp4.AnyTypeBox{Type: boxType("avcC")},
		Data: []byte{1, 0x4d, 0x40, 0x1e, 0xff, 0xe1, 0, 4, 0x67, 0x4d, 0x40, 0x1e}},
		mp4.Context{})
	_, _ = w.EndBox()

	if spec.TrackCrop != nil {
		_, _ = w.StartBox(&mp4.BoxInfo{Type: boxType("clap")})
		_, _ = mp4.Marshal(w, newFixed("clap", 32, clapPayload(spec.TrackCrop)), mp4.Context{})
		_, _ = w.EndBox()
	}
	if spec.TrackColor != nil {
		_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeColr()})
		c := spec.TrackColor
		c.Source = "track"
		_, _ = mp4.Marshal(w, colrBox(c), mp4.Context{})
		_, _ = w.EndBox()
	}
	_, _ = w.EndBox() // avc1
	_, _ = w.EndBox() // stsd
}

func writeAudioStsd(w *mp4.Writer, spec FixtureSpec) {
	rate, ch := spec.AudioSampleRate, spec.AudioChannels
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsd()})
	_, _ = mp4.Marshal(w, &mp4.Stsd{EntryCount: 1}, mp4.Context{})
	_, _ = w.StartBox(&mp4.BoxInfo{Type: boxType("mp4a")})
	declared := ch
	if spec.Damage == DamageBadLayout {
		// Declare more channels than the label set describes.
		declared = ch + 2
	}
	_, _ = mp4.Marshal(w, newFixed("mp4a", 28, mp4aPayload(rate, declared)),
		mp4.Context{})
	if len(spec.AudioLayout) > 0 {
		_, _ = w.StartBox(&mp4.BoxInfo{Type: boxType("chnl")})
		payload := []byte{}
		for i, l := range spec.AudioLayout {
			if i > 0 {
				payload = append(payload, 0)
			}
			payload = append(payload, []byte(l)...)
		}
		_, _ = mp4.Marshal(w, &RawBox{
			AnyTypeBox: mp4.AnyTypeBox{Type: boxType("chnl")},
			Data:       payload,
		}, mp4.Context{})
		_, _ = w.EndBox()
	}
	_, _ = w.EndBox()
	_, _ = w.EndBox()
}

func writeStts(w *mp4.Writer, count, delta uint32) {
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStts()})
	_, _ = mp4.Marshal(w, &mp4.Stts{EntryCount: 1,
		Entries: []mp4.SttsEntry{{SampleCount: count, SampleDelta: delta}}},
		mp4.Context{})
	_, _ = w.EndBox()
}

func writeStsc(w *mp4.Writer, count uint32) {
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsc()})
	_, _ = mp4.Marshal(w, &mp4.Stsc{EntryCount: 1,
		Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: count,
			SampleDescriptionIndex: 1}}}, mp4.Context{})
	_, _ = w.EndBox()
}

func writeStsz(w *mp4.Writer, count, size uint32) {
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsz()})
	sizes := make([]uint32, count)
	for i := range sizes {
		sizes[i] = size
	}
	_, _ = mp4.Marshal(w, &mp4.Stsz{SampleSize: 0, SampleCount: count,
		EntrySize: sizes}, mp4.Context{})
	_, _ = w.EndBox()
}

func writeStco(w *mp4.Writer, count uint32) {
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStco()})
	_, _ = mp4.Marshal(w, &mp4.Stco{EntryCount: count}, mp4.Context{})
	_, _ = w.EndBox()
}

func writeStss(w *mp4.Writer, spec FixtureSpec) {
	if spec.Damage == DamageMissingKeyframes {
		// Present an stss claiming zero sync samples: no editable entry.
		_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStss()})
		_, _ = mp4.Marshal(w, &mp4.Stss{EntryCount: 0}, mp4.Context{})
		_, _ = w.EndBox()
		return
	}
	var keys []uint32
	step := spec.KeyframeEvery
	if step == 0 {
		step = 1
	}
	for s := uint32(1); s <= spec.VideoSamples; s += step {
		keys = append(keys, s)
	}
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStss()})
	_, _ = mp4.Marshal(w, &mp4.Stss{EntryCount: uint32(len(keys)),
		SampleNumber: keys}, mp4.Context{})
	_, _ = w.EndBox()
}

// corruptSampleCount rewrites the first stsz SampleCount to a value
// larger than the real number of entries.
func corruptSampleCount(data []byte) []byte {
	marker := []byte{'s', 't', 's', 'z'}
	idx := indexOf(data, marker)
	// Layout after the fourCC: 4-byte box size, version(1)+flags(3),
	// sample_size(4), sample_count(4). SampleCount sits at marker+12.
	if idx < 0 || idx+16 >= len(data) {
		return data
	}
	out := append([]byte(nil), data...)
	binary.BigEndian.PutUint32(out[idx+12:],
		binary.BigEndian.Uint32(out[idx+12:])+50)
	return out
}

func indexOf(haystack, needle []byte) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
