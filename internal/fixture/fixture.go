// Package fixture self-generates MP4, caption and RTP samples for
// tests and the mediacheck demo entry point. No preset files needed.
package fixture

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/abema/go-mp4"
	"github.com/pion/rtp"

	"mediacheck/internal/timeline"
)

// MP4Spec describes the synthetic MP4 to generate.
type MP4Spec struct {
	Samples   int   // total video samples
	GOP       int   // keyframe interval in samples
	Timescale int64 // e.g. 90000
	Delta     int64 // sample duration in timescale units, e.g. 3003
}

// SampleBytes is the per-sample payload size; 120 samples ≈ 3.75 MiB
// so the default fixture spans several 1 MiB pipeline chunks.
const SampleBytes = 32 * 1024

// DefaultMP4Spec is 30000/1001 fps on a 90 kHz timescale.
func DefaultMP4Spec() MP4Spec {
	return MP4Spec{Samples: 120, GOP: 30, Timescale: 90000, Delta: 3003}
}

// KeyframeTimes returns the expected keyframe presentation times (ns).
func (s MP4Spec) KeyframeTimes() []int64 {
	var out []int64
	for i := 0; i < s.Samples; i += s.GOP {
		out = append(out, int64(i)*s.Delta*timeline.NanosPerSecond/s.Timescale)
	}
	return out
}

// WriteMP4 writes a minimal MP4 (ftyp, mdat, moov) with one video
// track to path and returns the expected keyframe times in ns.
func WriteMP4(path string, s MP4Spec) ([]int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	w := mp4.NewWriter(f)
	ctx := mp4.Context{}
	// leaf writes one complete box: header (via StartBox) + payload.
	leaf := func(t mp4.BoxType, box mp4.IImmutableBox) error {
		if _, err := w.StartBox(&mp4.BoxInfo{Type: t}); err != nil {
			return err
		}
		if _, err := mp4.Marshal(w, box, ctx); err != nil {
			return err
		}
		_, err := w.EndBox()
		return err
	}
	if err := leaf(mp4.BoxTypeFtyp(), &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 1,
	}); err != nil {
		return nil, err
	}
	// mdat first so the chunk offset is known when moov is written.
	mdat, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()})
	if err != nil {
		return nil, err
	}
	payloadOff := int64(mdat.Offset) + int64(mdat.HeaderSize)
	sample := make([]byte, SampleBytes)
	for i := 0; i < s.Samples; i++ {
		binary.BigEndian.PutUint32(sample, uint32(i))
		if _, err := w.Write(sample); err != nil {
			return nil, err
		}
	}
	if _, err := w.EndBox(); err != nil {
		return nil, err
	}

	dur := uint32(s.Delta * int64(s.Samples))
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return nil, err
	}
	if err := leaf(mp4.BoxTypeMvhd(), &mp4.Mvhd{Timescale: 1000, DurationV0: dur / 90}); err != nil {
		return nil, err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return nil, err
	}
	if err := leaf(mp4.BoxTypeTkhd(), &mp4.Tkhd{TrackID: 1, DurationV0: dur / 90}); err != nil {
		return nil, err
	}
	for _, b := range []mp4.BoxType{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl()} {
		if _, err := w.StartBox(&mp4.BoxInfo{Type: b}); err != nil {
			return nil, err
		}
		if b == mp4.BoxTypeMdia() {
			if err := leaf(mp4.BoxTypeMdhd(), &mp4.Mdhd{
				Timescale:  uint32(s.Timescale),
				DurationV0: dur,
			}); err != nil {
				return nil, err
			}
		}
	}
	syncs := []uint32{}
	for i := 0; i < s.Samples; i += s.GOP {
		syncs = append(syncs, uint32(i+1))
	}
	leaves := []struct {
		t mp4.BoxType
		b mp4.IImmutableBox
	}{
		{mp4.BoxTypeStsd(), &mp4.Stsd{EntryCount: 0}},
		{mp4.BoxTypeStts(), &mp4.Stts{EntryCount: 1, Entries: []mp4.SttsEntry{
			{SampleCount: uint32(s.Samples), SampleDelta: uint32(s.Delta)}}}},
		{mp4.BoxTypeStss(), &mp4.Stss{EntryCount: uint32(len(syncs)), SampleNumber: syncs}},
		{mp4.BoxTypeStsc(), &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{
			{FirstChunk: 1, SamplesPerChunk: uint32(s.Samples), SampleDescriptionIndex: 1}}}},
		{mp4.BoxTypeStsz(), &mp4.Stsz{SampleSize: SampleBytes, SampleCount: uint32(s.Samples)}},
		{mp4.BoxTypeStco(), &mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{uint32(payloadOff)}}},
	}
	for _, l := range leaves {
		if err := leaf(l.t, l.b); err != nil {
			return nil, err
		}
	}
	for i := 0; i < 5; i++ { // stbl, minf, mdia, trak, moov
		if _, err := w.EndBox(); err != nil {
			return nil, err
		}
	}
	return s.KeyframeTimes(), nil
}

// TruncateFile copies the first n bytes of src to dst.
func TruncateFile(src, dst string, n int64) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if n > int64(len(data)) {
		n = int64(len(data))
	}
	return os.WriteFile(dst, data[:n], 0o644)
}

// WriteVTT writes a WebVTT file. With bad=true it adds a reversed-time
// cue and a truncated trailing block.
func WriteVTT(path string, bad bool) error {
	content := "WEBVTT\n\n" +
		"00:00:00.000 --> 00:00:01.500\nopening shot\n\n" +
		"00:00:01.500 --> 00:00:03.000\nwide angle\n\n" +
		"00:00:03.000 --> 00:00:04.500\nplayer cam\n"
	if bad {
		content += "\n00:00:06.000 --> 00:00:05.000\nreversed time\n\n7\n"
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// WriteRTP writes length-prefixed RTP packets: in-order when clean,
// otherwise with a swap, a drop and one late duplicate.
func WriteRTP(path string, clean bool) error {
	const base = 100
	const count = 24
	mk := func(seq uint16) []byte {
		p := &rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 3000, SSRC: 0xdecaf},
			Payload: []byte{byte(seq)},
		}
		b, _ := p.Marshal()
		out := make([]byte, 2+len(b))
		binary.BigEndian.PutUint16(out, uint16(len(b)))
		copy(out[2:], b)
		return out
	}
	var order []uint16
	for i := 0; i < count; i++ {
		order = append(order, base+uint16(i))
	}
	if !clean {
		order[5], order[6] = order[6], order[5]   // reorder inside window
		order = append(order[:2], order[3:]...)   // drop seq 102
		order = append(order, base+8)             // late duplicate
	}
	var buf []byte
	for _, s := range order {
		buf = append(buf, mk(s)...)
	}
	return os.WriteFile(path, buf, 0o644)
}

var _ = fmt.Sprintf
