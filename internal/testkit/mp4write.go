package testkit

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"

	mp4 "github.com/abema/go-mp4"
)

// SampleSpec describes one sample to write into a fixture MP4.
type SampleSpec struct {
	Duration  int64
	Keyframe  bool
	Size      int
	ZeroBytes int // leading zero run, to manufacture a corrupt sample
}

// TrackSpec describes a single-track fixture file.
type TrackSpec struct {
	TrackID    uint32
	Kind       string // "video" or "audio"
	Codec      string // "avc1" or "mp4a"
	Timescale  uint32
	Width      uint16
	Height     uint16
	Channels   uint16
	SampleRate uint32
	Seed       uint64
	Samples    []SampleSpec
}

// Sample payloads are pseudo-random so a zero run is unambiguously damage.
func fillPayload(seed uint64, n int) []byte {
	out := make([]byte, n)
	s := seed | 1
	for i := range out {
		s ^= s << 13
		s ^= s >> 7
		s ^= s << 17
		out[i] = byte(s)
	}
	return out
}

// WriteMP4 writes one single-track fixture file and returns sample offsets.
func WriteMP4(path string, spec TrackSpec) ([]int64, error) {
	if spec.Timescale == 0 {
		return nil, fmt.Errorf("timescale must be set")
	}
	payloads := make([][]byte, len(spec.Samples))
	var total int
	for i, s := range spec.Samples {
		if s.Size <= 0 {
			return nil, fmt.Errorf("sample %d has non-positive size", i)
		}
		buf := fillPayload(spec.Seed+uint64(i)*2654435761, s.Size)
		if s.ZeroBytes > 0 {
			if s.ZeroBytes > len(buf) {
				s.ZeroBytes = len(buf)
			}
			for j := 0; j < s.ZeroBytes; j++ {
				buf[j] = 0
			}
		}
		payloads[i] = buf
		total += len(buf)
	}

	ftyp := marshalBox(&mp4.Ftyp{
		MajorBrand:       mp4.BrandISO6(),
		MinorVersion:     0,
		CompatibleBrands: []mp4.CompatibleBrandElem{{CompatibleBrand: mp4.BrandISO6()}},
	})
	moovFor := func(mdatOffset uint32) []byte {
		return buildMoov(spec, mdatOffset)
	}
	placeholder := moovFor(0)
	mdatOffset := uint32(len(ftyp) + len(placeholder) + 8)
	moov := moovFor(mdatOffset)
	if len(moov) != len(placeholder) {
		return nil, fmt.Errorf("moov size changed when patching chunk offsets")
	}

	var out bytes.Buffer
	out.Write(ftyp)
	out.Write(moov)
	out.Write(boxHeader(mp4.BoxTypeMdat(), uint32(total)))
	offsets := make([]int64, len(payloads))
	base := int64(len(ftyp) + len(moov) + 8)
	for i, p := range payloads {
		offsets[i] = base
		out.Write(p)
		base += int64(len(p))
	}
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		return nil, err
	}
	return offsets, nil
}

// Truncate cuts a file down to n bytes to manufacture a truncated container.
func Truncate(path string, n int64) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if n >= fi.Size() {
		return nil
	}
	return os.Truncate(path, n)
}

func buildMoov(spec TrackSpec, mdatOffset uint32) []byte {
	var stts []mp4.SttsEntry
	for _, s := range spec.Samples {
		d := uint32(s.Duration)
		if n := len(stts); n > 0 && stts[n-1].SampleDelta == d {
			stts[n-1].SampleCount++
			continue
		}
		stts = append(stts, mp4.SttsEntry{SampleCount: 1, SampleDelta: d})
	}
	var keyframes []uint32
	allKey := true
	anyKey := false
	for i, s := range spec.Samples {
		if s.Keyframe {
			keyframes = append(keyframes, uint32(i+1))
			anyKey = true
		} else {
			allKey = false
		}
	}
	sizes := make([]uint32, len(spec.Samples))
	for i, s := range spec.Samples {
		sizes[i] = uint32(s.Size)
	}
	var duration uint64
	for _, s := range spec.Samples {
		duration += uint64(s.Duration)
	}

	var trak bytes.Buffer
	trak.Write(box(mp4.BoxTypeTkhd(), func(b *bytes.Buffer) {
		t := &mp4.Tkhd{TrackID: spec.TrackID, DurationV0: uint32(duration), Width: uint32(spec.Width) << 16, Height: uint32(spec.Height) << 16}
		if spec.Kind == "audio" {
			t.Volume = 0x0100
		}
		writeFullBox(b, t)
	}))
	media := &bytes.Buffer{}
	media.Write(box(mp4.BoxTypeMdhd(), func(b *bytes.Buffer) {
		writeFullBox(b, &mp4.Mdhd{Timescale: spec.Timescale, DurationV0: uint32(duration)})
	}))
	handler := [4]byte{'v', 'i', 'd', 'e'}
	if spec.Kind == "audio" {
		handler = [4]byte{'s', 'o', 'u', 'n'}
	}
	media.Write(box(mp4.BoxTypeHdlr(), func(b *bytes.Buffer) {
		writeFullBox(b, &mp4.Hdlr{HandlerType: handler, Name: spec.Kind + "Handler"})
	}))
	media.Write(box(mp4.BoxTypeMinf(), func(b *bytes.Buffer) {
		b.Write(box(mp4.BoxTypeStbl(), func(b *bytes.Buffer) {
			b.Write(buildStsd(spec))
			b.Write(box(mp4.BoxTypeStts(), func(b *bytes.Buffer) {
				writeFullBox(b, &mp4.Stts{EntryCount: uint32(len(stts)), Entries: stts})
			}))
			if anyKey && !allKey {
				b.Write(box(mp4.BoxTypeStss(), func(b *bytes.Buffer) {
					writeFullBox(b, &mp4.Stss{EntryCount: uint32(len(keyframes)), SampleNumber: keyframes})
				}))
			}
			b.Write(box(mp4.BoxTypeStsz(), func(b *bytes.Buffer) {
				writeFullBox(b, &mp4.Stsz{SampleCount: uint32(len(sizes)), EntrySize: sizes})
			}))
			b.Write(box(mp4.BoxTypeStsc(), func(b *bytes.Buffer) {
				writeFullBox(b, &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: uint32(len(sizes)), SampleDescriptionIndex: 1}}})
			}))
			b.Write(box(mp4.BoxTypeStco(), func(b *bytes.Buffer) {
				writeFullBox(b, &mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{mdatOffset}})
			}))
		}))
	}))
	trak.Write(box(mp4.BoxTypeMdia(), func(b *bytes.Buffer) { b.Write(media.Bytes()) }))

	var moov bytes.Buffer
	moov.Write(box(mp4.BoxTypeMvhd(), func(b *bytes.Buffer) {
		writeFullBox(b, &mp4.Mvhd{Timescale: spec.Timescale, DurationV0: uint32(duration), Rate: 0x00010000, NextTrackID: spec.TrackID + 1})
	}))
	moov.Write(box(mp4.BoxTypeTrak(), func(b *bytes.Buffer) { b.Write(trak.Bytes()) }))
	return box(mp4.BoxTypeMoov(), func(b *bytes.Buffer) { b.Write(moov.Bytes()) })
}

func buildStsd(spec TrackSpec) []byte {
	var entry []byte
	if spec.Kind == "audio" {
		entry = box(mp4.BoxTypeMp4a(), func(b *bytes.Buffer) {
			ase := &mp4.AudioSampleEntry{ChannelCount: spec.Channels, SampleSize: 16, SampleRate: spec.SampleRate << 16}
			ase.SetType(mp4.BoxTypeMp4a())
			if _, err := mp4.Marshal(b, ase, mp4.Context{}); err != nil {
				panic(err)
			}
		})
	} else {
		entry = box(mp4.BoxTypeAvc1(), func(b *bytes.Buffer) {
			vse := &mp4.VisualSampleEntry{
				Width: spec.Width, Height: spec.Height, FrameCount: 1,
				Horizresolution: 0x00480000, Vertresolution: 0x00480000, Depth: 0x0018,
				Compressorname: [32]byte{4, 'x', '2', '6', '4'},
			}
			vse.SetType(mp4.BoxTypeAvc1())
			if _, err := mp4.Marshal(b, vse, mp4.Context{}); err != nil {
				panic(err)
			}
			avcC := &mp4.AVCDecoderConfiguration{ConfigurationVersion: 1, Profile: 66, Level: 30, LengthSizeMinusOne: 3}
			avcC.SetType(mp4.BoxTypeAvcC())
			b.Write(box(mp4.BoxTypeAvcC(), func(b *bytes.Buffer) {
				if _, err := mp4.Marshal(b, avcC, mp4.Context{}); err != nil {
					panic(err)
				}
			}))
		})
	}
	return box(mp4.BoxTypeStsd(), func(b *bytes.Buffer) {
		writeFullBox(b, &mp4.Stsd{EntryCount: 1})
		b.Write(entry)
	})
}

// boxHeader encodes a plain 32-bit box header.
func boxHeader(typ mp4.BoxType, payload uint32) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint32(out, payload+8)
	copy(out[4:], typ[:])
	return out
}

// box wraps the payload produced by fill in a box header.
func box(typ mp4.BoxType, fill func(*bytes.Buffer)) []byte {
	var payload bytes.Buffer
	fill(&payload)
	return append(boxHeader(typ, uint32(payload.Len())), payload.Bytes()...)
}

// writeFullBox marshals a full box payload including version and flags.
func writeFullBox(b *bytes.Buffer, src mp4.IImmutableBox) {
	if _, err := mp4.Marshal(b, src, mp4.Context{}); err != nil {
		panic(err)
	}
}

// marshalBox emits a complete leaf box.
func marshalBox(src mp4.IImmutableBox) []byte {
	var payload bytes.Buffer
	if _, err := mp4.Marshal(&payload, src, mp4.Context{}); err != nil {
		panic(err)
	}
	return append(boxHeader(src.GetType(), uint32(payload.Len())), payload.Bytes()...)
}
