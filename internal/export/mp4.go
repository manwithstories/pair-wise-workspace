// Package export writes the deterministic frame sequence into a fragmented
// MP4 container using github.com/Eyevinn/mp4ff. Audio is 16-bit little-endian
// PCM carried in mp4a sample entries; every render frame becomes one fragment,
// so the MP4 timeline mirrors the 30 fps frame boundaries one to one.
package export

import (
	"io"

	"github.com/Eyevinn/mp4ff/mp4"

	"avcheck/internal/clock"
	"avcheck/internal/render"
)

// Summary describes what was written, in a stable textual form.
type Summary struct {
	Bytes          uint64
	FragmentCount  int
	Timescale      uint32
	FirstDTS       uint64
	LastDTS        uint64
	SamplesPerFrame uint32
}

// Exporter renders fragmented MP4 output.
type Exporter struct {
	timescale uint32
	channels  uint16
}

// NewExporter builds an exporter on the given MP4 timescale and channel count.
func NewExporter(timescale uint32, channels int) *Exporter {
	if timescale == 0 {
		timescale = clock.SampleRate
	}
	return &Exporter{timescale: timescale, channels: uint16(channels)}
}

// Write encodes the sequence as ftyp+moov followed by one fragment per frame.
func (e *Exporter) Write(w io.Writer, seq *render.Sequence, mixedSamples []int32,
	channels int) (*Summary, error) {
	init := mp4.CreateEmptyInit()
	init.AddEmptyTrack(e.timescale, "audio", "und")
	trak := init.Moov.Trak
	entry := mp4.CreateAudioSampleEntryBox("mp4a",
		e.channels, 16, uint16(clock.SampleRate), nil)
	stsd := trak.Mdia.Minf.Stbl.Stsd
	stsd.AddChild(entry)

	if err := init.Encode(w); err != nil {
		return nil, err
	}

	perFrame := uint32(clock.TicksPerFrame())
	sum := &Summary{Timescale: e.timescale, SamplesPerFrame: perFrame}
	seg := mp4.NewMediaSegmentWithoutStyp()
	for i, fr := range seq.Frames {
		frag, err := mp4.CreateFragment(uint32(i+1), 1)
		if err != nil {
			return nil, err
		}
		start := fr.Index * int(perFrame)
		payload := pcm16LE(mixedSamples[start*channels : (start+int(perFrame))*channels])
		fs := mp4.FullSample{
			Sample: mp4.Sample{
				Flags:                0,
				Dur:                  perFrame,
				Size:                 uint32(len(payload)),
				CompositionTimeOffset: 0,
			},
			DecodeTime: fr.MP4Timestamp,
			Data:       payload,
		}
		frag.AddFullSample(fs)
		seg.AddFragment(frag)
		sum.FragmentCount++
		sum.Bytes += uint64(frag.Size())
		if i == 0 {
			sum.FirstDTS = fr.MP4Timestamp
		}
		sum.LastDTS = fr.MP4Timestamp
	}
	if err := seg.Encode(w); err != nil {
		return nil, err
	}
	sum.Bytes += uint64(init.Size()) + uint64(seg.Size())
	return sum, nil
}

// pcm16LE converts int32 samples to clipped 16-bit little-endian PCM.
func pcm16LE(in []int32) []byte {
	out := make([]byte, len(in)*2)
	for i, v := range in {
		s := int16(v)
		if int32(s) != v { // saturate on overflow
			if v > 0 {
				s = 32767
			} else {
				s = -32768
			}
		}
		u := uint16(s)
		out[i*2] = byte(u)
		out[i*2+1] = byte(u >> 8)
	}
	return out
}

// Line renders the summary as deterministic key=value text.
func (s *Summary) Line() string {
	return "mp4_fragments=" + itoa(s.FragmentCount) +
		" mp4_timescale=" + utoa(uint64(s.Timescale)) +
		" mp4_first_dts=" + utoa(s.FirstDTS) +
		" mp4_last_dts=" + utoa(s.LastDTS) +
		" mp4_bytes=" + utoa(s.Bytes)
}

func itoa(n int) string { return utoa(uint64(n)) }

func utoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
