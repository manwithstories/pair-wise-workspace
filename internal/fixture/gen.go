// Package fixture generates all demo samples locally: SRT/WebVTT subtitle
// tracks, device timecode lists, RTP timestamp streams and MP4 files with
// controllable damage (tail truncation, incomplete index). No external
// media is referenced.
package fixture

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	mp4 "github.com/abema/go-mp4"
	"github.com/pion/rtp"

	"review/internal/clock"
)

// SampleSize is the fixed MP4 sample payload size used by the generator.
const SampleSize = 64

// seekBuf is an in-memory io.WriteSeeker for two-pass MP4 layout.
type seekBuf struct {
	buf []byte
	off int64
}

func (s *seekBuf) Write(p []byte) (int, error) {
	end := s.off + int64(len(p))
	if end > int64(len(s.buf)) {
		s.buf = append(s.buf, make([]byte, end-int64(len(s.buf)))...)
	}
	copy(s.buf[s.off:], p)
	s.off = end
	return len(p), nil
}

func (s *seekBuf) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0:
		s.off = offset
	case 1:
		s.off += offset
	case 2:
		s.off = int64(len(s.buf)) + offset
	}
	return s.off, nil
}

// writeMP4 serialises ftyp + moov + mdat with the given chunk offsets.
func writeMP4(w *mp4.Writer, samples int, r clock.Rate, offsets []uint32, cam int) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return err
	}
	ftyp := &mp4.Ftyp{MajorBrand: [4]byte{'i', 's', 'o', 'm'}}
	ftyp.AddCompatibleBrand([4]byte{'i', 's', 'o', 'm'})
	if _, err := mp4.Marshal(w, ftyp, mp4.Context{}); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return err
	}
	for _, t := range []mp4.BoxType{mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl()} {
		if _, err := w.StartBox(&mp4.BoxInfo{Type: t}); err != nil {
			return err
		}
	}
	sizes := make([]uint32, samples)
	for i := range sizes {
		sizes[i] = SampleSize
	}
	boxTypes := []mp4.BoxType{mp4.BoxTypeStts(), mp4.BoxTypeStsc(), mp4.BoxTypeStsz(), mp4.BoxTypeStco()}
	boxes := []mp4.IImmutableBox{
		&mp4.Stts{EntryCount: 1, Entries: []mp4.SttsEntry{{SampleCount: uint32(samples), SampleDelta: uint32(r.Den)}}},
		&mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: 1, SampleDescriptionIndex: 1}}},
		&mp4.Stsz{SampleCount: uint32(samples), EntrySize: sizes},
		&mp4.Stco{EntryCount: uint32(len(offsets)), ChunkOffset: offsets},
	}
	for i, b := range boxes {
		if _, err := w.StartBox(&mp4.BoxInfo{Type: boxTypes[i]}); err != nil {
			return err
		}
		if _, err := mp4.Marshal(w, b, mp4.Context{}); err != nil {
			return err
		}
		if _, err := w.EndBox(); err != nil {
			return err
		}
	}
	for i := 0; i < 4; i++ {
		if _, err := w.EndBox(); err != nil {
			return err
		}
	}
	if _, err := w.EndBox(); err != nil { // moov
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return err
	}
	data := make([]byte, samples*SampleSize)
	for i := range data {
		data[i] = byte(i/SampleSize*7 + cam*13 + i%SampleSize)
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

// GenMP4 writes a deterministic MP4 with one sample per chunk.
// damage "truncated" cuts the tail inside mdat; "incomplete-index" declares
// fewer chunk offsets than samples. It returns the exact damage offset for
// "truncated" and -1 otherwise (the inspector derives the offset itself).
func GenMP4(path string, samples int, r clock.Rate, cam int, damage string) (int64, error) {
	// Pass 1: measure the moov size with zero offsets.
	probe := &seekBuf{}
	if err := writeMP4(mp4.NewWriter(probe), samples, r, make([]uint32, samples), cam); err != nil {
		return -1, err
	}
	mdatDataStart := int64(len(probe.buf)) - int64(samples*SampleSize)
	offsets := make([]uint32, samples)
	declared := samples
	if damage == "incomplete-index" {
		declared = samples - 3
	}
	for i := 0; i < declared; i++ {
		offsets[i] = uint32(mdatDataStart) + uint32(i*SampleSize)
	}
	// Pass 2: write the real file.
	final := &seekBuf{}
	if err := writeMP4(mp4.NewWriter(final), samples, r, offsets[:declared], cam); err != nil {
		return -1, err
	}
	data := final.buf
	damageOff := int64(-1)
	if damage == "truncated" {
		damageOff = mdatDataStart + int64(samples*7/10)*SampleSize
		data = data[:damageOff]
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return -1, err
	}
	return damageOff, os.WriteFile(path, data, 0o644)
}

// GenRTP writes a length-prefixed RTP stream whose 32-bit timestamps start
// just below the wrap point. With anomalies on, packet 10 duplicates the
// previous timestamp, packet 20 jumps backward and packet 30 is dropped.
func GenRTP(path string, frames int, r clock.Rate, anomalies bool) error {
	const clockRate = 90000
	delta := uint32(clockRate * r.Den / r.Num)
	base := uint32(0xFFFFFFF0) - 2*delta
	var out bytes.Buffer
	for i := 0; i < frames; i++ {
		if anomalies && i == 30 {
			continue // dropped packet -> gap (sequence number jumps)
		}
		ts := base + uint32(i)*delta
		switch {
		case anomalies && i == 10:
			ts = base + 9*delta // duplicate of packet 9
		case anomalies && i == 20:
			ts = base + 10*delta // backward glitch
		}
		p := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96,
			SequenceNumber: uint16(1000 + i), Timestamp: ts, SSRC: 0x11223344},
			Payload: []byte{byte(i)}}
		raw, err := p.Marshal()
		if err != nil {
			return err
		}
		var hdr [2]byte
		binary.BigEndian.PutUint16(hdr[:], uint16(len(raw)))
		out.Write(hdr[:])
		out.Write(raw)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, out.Bytes(), 0o644)
}


// GenSRT returns a deterministic SRT track. With dirty on, one invalid-time
// record and one overlapping cue are injected mid-file. With truncated on,
// the file is cut mid-record at the tail.
func GenSRT(cam int, r clock.Rate, cues int, dirty, truncated bool) []byte {
	var b bytes.Buffer
	for i := 0; i < cues; i++ {
		start := int64(i*2)*r.Nominal() + r.Nominal()/2
		end := start + r.Nominal()*3/2
		if dirty && i == 8 {
			start -= r.Nominal() // overlaps the previous cue
		}
		fmt.Fprintf(&b, "%d\n%s --> %s\ncam%d cue %d\n\n", i+1,
			srtTime(r.FramesToSeconds(start), ','),
			srtTime(r.FramesToSeconds(end), ','), cam, i)
		if dirty && i == 4 {
			b.WriteString("900\n00:00:99,000 --> 00:00:98,000\nbroken time\n\n")
		}
	}
	data := b.Bytes()
	if truncated {
		data = data[:len(data)-len(data)/20-3] // cut mid-line at the tail
	}
	return data
}

// GenVTT returns a deterministic WebVTT track.
func GenVTT(cam int, r clock.Rate, cues int) []byte {
	var b bytes.Buffer
	b.WriteString("WEBVTT\n\n")
	for i := 0; i < cues; i++ {
		start := int64(i*4+1) * r.Nominal()
		end := start + r.Nominal()*2
		fmt.Fprintf(&b, "cue-%d\n%s --> %s\ncam%d vtt %d\n\n", i,
			srtTime(r.FramesToSeconds(start), '.'),
			srtTime(r.FramesToSeconds(end), '.'), cam, i)
	}
	return b.Bytes()
}

// srtTime formats seconds as "HH:MM:SS<sep>mmm".
func srtTime(sec float64, sep byte) string {
	ms := int64(sec*1000 + 0.5)
	return fmt.Sprintf("%02d:%02d:%02d%c%03d", ms/3600000, ms/60000%60, ms/1000%60, sep, ms%1000)
}

// GenTimecodes returns a device timecode CSV starting at 23:59:50 so the
// list crosses midnight, plus one unconvertible record.
func GenTimecodes(r clock.Rate, seconds int) []byte {
	var b bytes.Buffer
	startTC := clock.Timecode{HH: 23, MM: 59, SS: 50, FF: 0, Drop: r.DropSupported()}
	base, _ := startTC.ToFrames(r)
	for i := 0; i < seconds; i++ {
		f := base + int64(i)*r.Nominal()
		tc, _ := clock.FormatTimecode(f, r, r.DropSupported())
		fmt.Fprintf(&b, "%d,%s\n", i, tc)
	}
	fmt.Fprintf(&b, "%d,25:99:99:99\n", seconds) // unconvertible record
	return b.Bytes()
}
