package remux

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"archivecheck.local/archivecheck/internal/clock"
	"archivecheck.local/archivecheck/internal/fault"
	"archivecheck.local/archivecheck/internal/track"
)

// Write serialises the plan as an MP4 file: ftyp, mdat, moov.
//
// The moov box is written after mdat so every chunk offset is exact. Sample
// payloads are copied byte-for-byte from the source ranges recorded by the
// demuxer; no codec is transcoded. A plan containing a track without a
// documented MP4 mapping is rejected by Validate before anything is written.
func Write(w io.WriteSeeker, p *Plan) (*Summary, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	SortTracks(p.Tracks)
	sum := &Summary{
		TrackSamples:  map[uint64]int64{},
		ChunkOffsets:  map[uint64][]int64{},
		SampleSizes:   map[uint64][]int32{},
		SampleOffsets: map[uint64][]int64{},
		Keyframes:     map[uint64][]uint32{},
	}
	bw := &writer{w: w}

	brand := "isom"
	if len(p.Brand) == 4 {
		brand = p.Brand
	}
	if err := writeFtyp(bw, brand); err != nil {
		return nil, err
	}

	mdatStart := bw.pos
	if err := bw.box64("mdat", func() error {
		return writeMedia(bw, p, sum)
	}); err != nil {
		return nil, err
	}
	sum.MdatOffset = mdatStart

	if err := bw.box("moov", func() error {
		if err := writeMvhd(bw, p); err != nil {
			return err
		}
		for _, t := range p.Tracks {
			if err := writeTrak(bw, t, sum); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sum.Size = bw.pos
	return sum, nil
}

// outputChunkSize groups samples into stsc/stco chunks. Samples never span
// chunks and each chunk is one contiguous mdat run.
const outputChunkSize = 10

func writeMedia(bw *writer, p *Plan, sum *Summary) error {
	for _, t := range p.Tracks {
		for _, ch := range planChunks(len(t.Samples), outputChunkSize) {
			sum.ChunkOffsets[t.Track.Number] = append(sum.ChunkOffsets[t.Track.Number], bw.pos)
			for i := 0; i < ch.count; i++ {
				s := t.Samples[ch.firstSample+i]
				off := bw.pos
				if _, err := copySample(bw, s); err != nil {
					return err
				}
				n := uint32(ch.firstSample + i + 1)
				sum.SampleOffsets[t.Track.Number] = append(sum.SampleOffsets[t.Track.Number], off)
				sum.SampleSizes[t.Track.Number] = append(sum.SampleSizes[t.Track.Number], int32(sampleSize(s)))
				sum.TrackSamples[t.Track.Number] = int64(n)
				if s.Keyframe {
					sum.Keyframes[t.Track.Number] = append(sum.Keyframes[t.Track.Number], n)
				}
				sum.Manifest = append(sum.Manifest, ManifestEntry{
					Track:        t.Track.Number,
					OutputSample: n,
					SourceOffset: s.SourceOffset,
					Size:         int(sampleSize(s)),
					Keyframe:     s.Keyframe,
				})
			}
		}
	}
	return nil
}

func writeFtyp(bw *writer, brand string) error {
	body := make([]byte, 0, 20)
	body = append(body, brand...)
	body = binary.BigEndian.AppendUint32(body, 0x200)
	body = append(body, brand...)
	body = append(body, "iso6"...)
	body = append(body, "mp41"...)
	return bw.rawBox("ftyp", body)
}

func writeMvhd(bw *writer, p *Plan) error {
	dur := movieDuration(p)
	return bw.leaf(&mp4.Mvhd{
		Timescale:   uint32(p.MovieTimescale),
		DurationV0:  uint32(dur),
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      identityMatrix(),
		NextTrackID: uint32(len(p.Tracks) + 1),
	})
}

func identityMatrix() [9]int32 {
	return [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
}

func movieDuration(p *Plan) int64 {
	var maxNS int64
	for _, t := range p.Tracks {
		if ns := trackDurationNS(t); ns > maxNS {
			maxNS = ns
		}
	}
	return clock.NanosToTicks(maxNS, p.MovieTimescale)
}

func writeTrak(bw *writer, t *TrackPlan, sum *Summary) error {
	return bw.box("trak", func() error {
		tkhd := &mp4.Tkhd{
			TrackID:    t.Track.DestTrackID,
			DurationV0: uint32(clock.NanosToTicks(trackDurationNS(t), t.TimescaleMovie)),
			Matrix:     identityMatrix(),
			Width:      uint32(t.Width) << 16,
			Height:     uint32(t.Height) << 16,
		}
		if t.Handler == "soun" {
			tkhd.Volume = 0x0100
		}
		if err := bw.leaf(tkhd); err != nil {
			return err
		}
		if t.Shift > 0 {
			if err := writeEdts(bw, t); err != nil {
				return err
			}
		}
		return bw.box("mdia", func() error {
			if err := bw.leaf(&mp4.Mdhd{
				Timescale:  uint32(t.Timescale),
				DurationV0: uint32(trackDuration(t)),
				Language:   languageCode(t.Language),
			}); err != nil {
				return err
			}
			handler := t.Handler
			if err := bw.leaf(&mp4.Hdlr{
				PreDefined:  0,
				HandlerType: handlerType(handler),
				Name:        handlerName(handler),
			}); err != nil {
				return err
			}
			return bw.box("minf", func() error {
				if err := writeMediaHeader(bw, handler); err != nil {
					return err
				}
				if err := writeDinf(bw); err != nil {
					return err
				}
				return writeStbl(bw, t, sum)
			})
		})
	})
}

func handlerType(h string) [4]byte {
	switch h {
	case "vide":
		return [4]byte{'v', 'i', 'd', 'e'}
	case "soun":
		return [4]byte{'s', 'o', 'u', 'n'}
	default:
		return [4]byte{'s', 'b', 't', 'l'}
	}
}

func writeMediaHeader(bw *writer, handler string) error {
	switch handler {
	case "vide":
		return bw.leaf(&mp4.Vmhd{})
	case "soun":
		return bw.leaf(&mp4.Smhd{})
	default:
		return bw.fullBox("nmhd", 0, 0, nil)
	}
}

func writeEdts(bw *writer, t *TrackPlan) error {
	// An empty edit of the shifted duration keeps the first sample's
	// presentation time where the source had it.
	shiftTicks := clock.NanosToTicks(t.Shift*clock.NanosPerSecond/t.Timescale, t.TimescaleMovie)
	_ = shiftTicks
	return bw.box("edts", func() error {
		return bw.box("elst", func() error {
			body := make([]byte, 0, 12)
			body = binary.BigEndian.AppendUint32(body, 0) // version/flags
			body = binary.BigEndian.AppendUint32(body, 1) // entry count
			body = binary.BigEndian.AppendUint32(body, uint32(clock.NanosToTicks(int64(t.Shift)*clock.NanosPerSecond/t.Timescale, t.TimescaleMovie)))
			body = binary.BigEndian.AppendUint32(body, uint32(0xFFFFFFFF)) // media_time -1: empty edit
			body = binary.BigEndian.AppendUint32(body, 0x00010000)         // rate 1.0
			return bw.write(body)
		})
	})
}

func writeDinf(bw *writer) error {
	return bw.box("dinf", func() error {
		return bw.box("dref", func() error {
			body := []byte{0, 0, 0, 0, 0, 0, 0, 1}
			if err := bw.write(body); err != nil {
				return err
			}
			return bw.fullBox("url ", 0, 1, nil)
		})
	})
}

func trackDuration(t *TrackPlan) int64 {
	return clock.NanosToTicks(trackDurationNS(t), t.Timescale)
}

func trackDurationNS(t *TrackPlan) int64 {
	n := len(t.Samples)
	if n == 0 {
		return 0
	}
	last := t.Samples[n-1]
	// Convert end ticks to nanoseconds without overflowing int64 in practice:
	// DTS and Duration are bounded by the source duration.
	return (last.DTS + last.Duration) * clock.NanosPerSecond / t.Timescale
}

// languageCode packs an ISO-639-2 code into the 15-bit mdhd field. Unknown or
// malformed codes become "und".
func languageCode(lang string) [3]byte {
	if len(lang) != 3 {
		return [3]byte{'u', 'n', 'd'}
	}
	for i := 0; i < 3; i++ {
		if lang[i] < 'a' || lang[i] > 'z' {
			return [3]byte{'u', 'n', 'd'}
		}
	}
	return [3]byte{lang[0], lang[1], lang[2]}
}

func handlerName(h string) string {
	switch h {
	case "vide":
		return "VideoHandler"
	case "soun":
		return "SoundHandler"
	default:
		return "SubtitleHandler"
	}
}

// writeStbl writes the sample table.
func writeStbl(bw *writer, t *TrackPlan, sum *Summary) error {
	return bw.box("stbl", func() error {
		if err := writeStsd(bw, t); err != nil {
			return err
		}
		if err := writeStts(bw, t); err != nil {
			return err
		}
		if err := writeStsc(bw, t); err != nil {
			return err
		}
		if err := writeStsz(bw, t); err != nil {
			return err
		}
		if err := writeStco(bw, sum.ChunkOffsets[t.Track.Number]); err != nil {
			return err
		}
		if t.Handler == "vide" {
			if err := writeStss(bw, sum.Keyframes[t.Track.Number]); err != nil {
				return err
			}
		}
		if hasOffsets(t) {
			if err := writeCtts(bw, t); err != nil {
				return err
			}
		}
		return nil
	})
}

func hasOffsets(t *TrackPlan) bool {
	for _, s := range t.Samples {
		if s.PTS != s.DTS {
			return true
		}
	}
	return false
}

func writeStsd(bw *writer, t *TrackPlan) error {
	return bw.box("stsd", func() error {
		body := make([]byte, 0, 8)
		body = binary.BigEndian.AppendUint32(body, 0)
		body = binary.BigEndian.AppendUint32(body, 1)
		if err := bw.write(body); err != nil {
			return err
		}
		return writeSampleEntry(bw, t)
	})
}

func writeSampleEntry(bw *writer, t *TrackPlan) error {
	m := t.Track.Mapping
	cfg := t.Track.CodecPrivate
	switch m.Kind {
	case track.KindAVC, track.KindHEVC, track.KindVP9, track.KindAV1:
		return bw.box(m.Entry, func() error {
			entry := &mp4.VisualSampleEntry{
				SampleEntry: mp4.SampleEntry{
					AnyTypeBox:         mp4.AnyTypeBox{Type: mp4.StrToBoxType(m.Entry)},
					DataReferenceIndex: 1,
				},
				Width:           uint16(t.Width),
				Height:          uint16(t.Height),
				Horizresolution: 0x00480000,
				Vertresolution:  0x00480000,
				FrameCount:      1,
				Compressorname:  compressName("archivecheck"),
				Depth:           0x0018,
			}
			if err := bw.payload(entry); err != nil {
				return err
			}
			if len(cfg) == 0 {
				return nil
			}
			if m.ConfigBox == "" {
				return nil
			}
			return bw.rawBox(m.ConfigBox, cfg)
		})
	case track.KindAAC, track.KindOpus, track.KindFLAC:
		return bw.box(m.Entry, func() error {
			entry := &mp4.AudioSampleEntry{
				SampleEntry: mp4.SampleEntry{
					AnyTypeBox:         mp4.AnyTypeBox{Type: mp4.StrToBoxType(m.Entry)},
					DataReferenceIndex: 1,
				},
				ChannelCount: uint16(t.Channels),
				SampleSize:   16,
				SampleRate:   uint32(t.SampleRate) << 16,
			}
			if err := bw.payload(entry); err != nil {
				return err
			}
			if len(cfg) == 0 {
				return nil
			}
			if m.Kind == track.KindAAC {
				return writeESDS(bw, cfg, uint32(t.SampleRate), uint16(t.Channels))
			}
			if m.ConfigBox == "" {
				return nil
			}
			return bw.rawBox(m.ConfigBox, cfg)
		})
	case track.KindText:
		return bw.box(m.Entry, func() error {
			entry := &mp4.TextSubtitleSampleEntry{
				SampleEntry: mp4.SampleEntry{
					AnyTypeBox:         mp4.AnyTypeBox{Type: mp4.StrToBoxType(m.Entry)},
					DataReferenceIndex: 1,
				},
				MIMEFormat: "text/plain",
			}
			return bw.payload(entry)
		})
	default:
		return fault.New(fault.StageRemux, fault.ErrUnsupportedCodec).OnTrack(t.Track.Number)
	}
}

func compressName(s string) [32]byte {
	var out [32]byte
	n := len(s)
	if n > 31 {
		n = 31
	}
	out[0] = byte(n)
	copy(out[1:], s[:n])
	return out
}

// writeESDS wraps an AAC AudioSpecificConfig. The config bytes are carried in
// the DecoderSpecificInfo descriptor without modification.
func writeESDS(bw *writer, asc []byte, sampleRate uint32, channels uint16) error {
	return bw.box("esds", func() error {
		if err := bw.write([]byte{0, 0, 0, 0}); err != nil { // version/flags
			return err
		}
		// ES_Descriptor (tag 0x03) wrapping DecoderConfigDescriptor (0x04).
		dec := buildDecoderConfig(asc, sampleRate, channels)
		es := []byte{0x00, 0x01, 0x00} // ES_ID, flags
		es = append(es, dec...)
		es = append(es, 0x06, 0x01, 0x02) // SLConfigDescriptor
		return bw.write(append([]byte{0x03}, append(descriptorLength(len(es)), es...)...))
	})
}

func buildDecoderConfig(asc []byte, sampleRate uint32, channels uint16) []byte {
	_ = sampleRate
	dcd := []byte{
		0x40,             // Audio ISO/IEC 14496-3
		0x15,             // streamType audio (5)<<2 | upstream 0 | reserved 1
		0x00, 0x00, 0x00, // bufferSizeDB
		0x00, 0x00, 0x00, 0x00, // maxBitrate
		0x00, 0x00, 0x00, 0x00, // avgBitrate
	}
	dcd = append(dcd, 0x05)
	dcd = append(dcd, descriptorLength(len(asc))...)
	dcd = append(dcd, asc...)
	return append([]byte{0x04}, append(descriptorLength(len(dcd)), dcd...)...)
}

// descriptorLength encodes a descriptor length with the MPEG-4 7-bit form.
func descriptorLength(n int) []byte {
	var out []byte
	for i := 3; i >= 0; i-- {
		b := byte(n >> (7 * uint(i)) & 0x7f)
		if i != 0 {
			b |= 0x80
		}
		out = append(out, b)
	}
	// Trim leading 0x80 continuation bytes for the minimal form.
	for len(out) > 1 && out[0] == 0x80 {
		out = out[1:]
	}
	return out
}

func writeStts(bw *writer, t *TrackPlan) error {
	entries := buildSttsEntries(t.Samples)
	return bw.box("stts", func() error {
		body := make([]byte, 0, 8+8*len(entries))
		body = binary.BigEndian.AppendUint32(body, 0)
		body = binary.BigEndian.AppendUint32(body, uint32(len(entries)))
		for _, e := range entries {
			body = binary.BigEndian.AppendUint32(body, e.count)
			body = binary.BigEndian.AppendUint32(body, uint32(e.delta))
		}
		return bw.write(body)
	})
}

type sttsEntry struct {
	count uint32
	delta int64
}

func buildSttsEntries(samples []Sample) []sttsEntry {
	var out []sttsEntry
	for _, s := range samples {
		delta := s.Duration
		if delta < 0 {
			delta = 0
		}
		if n := len(out); n > 0 && out[n-1].delta == delta {
			out[n-1].count++
			continue
		}
		out = append(out, sttsEntry{count: 1, delta: delta})
	}
	return out
}

func writeStsc(bw *writer, t *TrackPlan) error {
	chunks := planChunks(len(t.Samples), outputChunkSize)
	// All chunks have the same sample count except possibly the last, so at
	// most two stsc entries are needed.
	type entry struct {
		first, per uint32
	}
	var entries []entry
	for i, ch := range chunks {
		per := uint32(ch.count)
		if n := len(entries); n > 0 && entries[n-1].per == per {
			continue
		}
		entries = append(entries, entry{first: uint32(i + 1), per: per})
	}
	return bw.box("stsc", func() error {
		body := make([]byte, 0, 8+12*len(entries))
		body = binary.BigEndian.AppendUint32(body, 0)
		body = binary.BigEndian.AppendUint32(body, uint32(len(entries)))
		for _, e := range entries {
			body = binary.BigEndian.AppendUint32(body, e.first)
			body = binary.BigEndian.AppendUint32(body, e.per)
			body = binary.BigEndian.AppendUint32(body, 1) // sample description index
		}
		return bw.write(body)
	})
}

func writeStsz(bw *writer, t *TrackPlan) error {
	return bw.box("stsz", func() error {
		body := make([]byte, 0, 12+4*len(t.Samples))
		body = binary.BigEndian.AppendUint32(body, 0)
		body = binary.BigEndian.AppendUint32(body, 0) // variable sizes
		body = binary.BigEndian.AppendUint32(body, uint32(len(t.Samples)))
		for _, s := range t.Samples {
			body = binary.BigEndian.AppendUint32(body, uint32(sampleSize(s)))
		}
		return bw.write(body)
	})
}

func writeStco(bw *writer, offsets []int64) error {
	return bw.box("stco", func() error {
		body := make([]byte, 0, 8+4*len(offsets))
		body = binary.BigEndian.AppendUint32(body, 0)
		body = binary.BigEndian.AppendUint32(body, uint32(len(offsets)))
		for _, off := range offsets {
			if off > 0xFFFFFFFF {
				return fault.New(fault.StageRemux, fmt.Errorf("%w: chunk offset %d needs co64", fault.ErrOutput, off))
			}
			body = binary.BigEndian.AppendUint32(body, uint32(off))
		}
		return bw.write(body)
	})
}

func writeStss(bw *writer, keyframes []uint32) error {
	return bw.box("stss", func() error {
		body := make([]byte, 0, 8+4*len(keyframes))
		body = binary.BigEndian.AppendUint32(body, 0)
		body = binary.BigEndian.AppendUint32(body, uint32(len(keyframes)))
		for _, k := range keyframes {
			body = binary.BigEndian.AppendUint32(body, k)
		}
		return bw.write(body)
	})
}

func writeCtts(bw *writer, t *TrackPlan) error {
	// Version 1 permits negative composition offsets, which the mapper
	// preserves for PTS < DTS.
	type entry struct {
		count  uint32
		offset int32
	}
	var entries []entry
	for _, s := range t.Samples {
		off := int32(s.PTS - s.DTS)
		if n := len(entries); n > 0 && entries[n-1].offset == off {
			entries[n-1].count++
			continue
		}
		entries = append(entries, entry{count: 1, offset: off})
	}
	return bw.box("ctts", func() error {
		body := make([]byte, 0, 8+8*len(entries))
		body = binary.BigEndian.AppendUint32(body, 0x01000000) // version 1
		body = binary.BigEndian.AppendUint32(body, uint32(len(entries)))
		for _, e := range entries {
			body = binary.BigEndian.AppendUint32(body, e.count)
			body = binary.BigEndian.AppendUint32(body, uint32(e.offset))
		}
		return bw.write(body)
	})
}

// compile-time reference so the mp4 import is used even if sample entries
// change shape.
var _ = mp4.BoxTypeFtyp
