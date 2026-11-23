// Package segment writes fMP4 media segments on the unified time line and
// validates them by reading the box structure back. Segment boundaries
// fall only on real packet boundaries; loss gaps are skipped, never
// filled with fabricated media.
package segment

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	mp4 "github.com/abema/go-mp4"

	"mediarchive/internal/clock"
	"mediarchive/internal/event"
	"mediarchive/internal/safepath"
)

// DefaultDuration is the default segment length: 2 seconds.
const DefaultDuration = clock.Time(2 * clock.Rate)

// Info describes one written media segment.
type Info struct {
	Path     string
	Start    clock.Time
	End      clock.Time
	StartSeq uint64
	Samples  int
	Bytes    int64
	SHA256   string
}

// Write segments packets (repaired, sequence-ordered) into an init
// segment plus fMP4 media segments of approximately segDur under dir.
func Write(dir string, packets []*event.Packet, cfg event.CodecConfig, segDur clock.Time) ([]Info, error) {
	if len(packets) == 0 {
		return nil, &event.Error{Op: "segment.write", Reason: event.ReasonNoBoundary,
			Err: fmt.Errorf("no packets to segment")}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	last := packets[len(packets)-1]
	total := last.MediaTime + last.Duration - packets[0].MediaTime
	initPath, err := safepath.Join(dir, "init.mp4")
	if err != nil {
		return nil, err
	}
	if err := writeInit(initPath, cfg, total); err != nil {
		return nil, err
	}
	var infos []Info
	seqNum := uint32(1)
	start := 0
	segStart := packets[0].MediaTime
	flush := func(end int) error {
		info, err := writeSegment(dir, len(infos), seqNum, packets[start:end])
		if err != nil {
			return err
		}
		infos = append(infos, info)
		seqNum++
		return nil
	}
	for i, p := range packets {
		if i > start && p.MediaTime-segStart >= segDur {
			if err := flush(i); err != nil {
				return nil, err
			}
			start = i
			segStart = p.MediaTime
		}
	}
	if err := flush(len(packets)); err != nil {
		return nil, err
	}
	return infos, nil
}

// memWS is an in-memory io.WriteSeeker for box assembly.
type memWS struct {
	buf []byte
	off int64
}

func (m *memWS) Write(p []byte) (int, error) {
	end := m.off + int64(len(p))
	if end > int64(len(m.buf)) {
		nb := make([]byte, end)
		copy(nb, m.buf)
		m.buf = nb
	}
	copy(m.buf[m.off:end], p)
	m.off = end
	return len(p), nil
}

func (m *memWS) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = m.off + offset
	case io.SeekEnd:
		abs = int64(len(m.buf)) + offset
	}
	if abs < 0 {
		return 0, fmt.Errorf("negative seek offset")
	}
	m.off = abs
	return abs, nil
}

// leaf writes a complete leaf box: header, payload, size fix-up.
func leaf(w *mp4.Writer, t mp4.BoxType, box mp4.IImmutableBox) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: t}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, box, mp4.Context{}); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

// box writes a container box whose children are emitted by fill.
func box(w *mp4.Writer, t mp4.BoxType, fill func() error) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: t}); err != nil {
		return err
	}
	if err := fill(); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

func writeInit(path string, cfg event.CodecConfig, total clock.Time) error {
	mw := &memWS{}
	w := mp4.NewWriter(mw)
	ftyp := &mp4.Ftyp{MajorBrand: mp4.BrandISO8()}
	ftyp.AddCompatibleBrand(mp4.BrandISO8())
	ftyp.AddCompatibleBrand(mp4.BrandMP41())
	if err := leaf(w, mp4.BoxTypeFtyp(), ftyp); err != nil {
		return err
	}
	ident := [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
	err := box(w, mp4.BoxTypeMoov(), func() error {
		if err := leaf(w, mp4.BoxTypeMvhd(), &mp4.Mvhd{
			Timescale: clock.Rate, DurationV0: uint32(total),
			Rate: 0x00010000, Volume: 0x0100, Matrix: ident, NextTrackID: 2,
		}); err != nil {
			return err
		}
		if err := box(w, mp4.BoxTypeTrak(), func() error {
			tkhd := &mp4.Tkhd{TrackID: 1, DurationV0: uint32(total), Volume: 0x0100, Matrix: ident}
			tkhd.SetFlags(0x7)
			if err := leaf(w, mp4.BoxTypeTkhd(), tkhd); err != nil {
				return err
			}
			return box(w, mp4.BoxTypeMdia(), func() error {
				if err := leaf(w, mp4.BoxTypeMdhd(), &mp4.Mdhd{Timescale: clock.Rate, DurationV0: uint32(total)}); err != nil {
					return err
				}
				if err := leaf(w, mp4.BoxTypeHdlr(), &mp4.Hdlr{
					HandlerType: [4]byte{'s', 'o', 'u', 'n'}, Name: "SoundHandler",
				}); err != nil {
					return err
				}
				return writeMinf(w, cfg)
			})
		}); err != nil {
			return err
		}
		return box(w, mp4.BoxTypeMvex(), func() error {
			return leaf(w, mp4.BoxTypeTrex(), &mp4.Trex{TrackID: 1, DefaultSampleDescriptionIndex: 1})
		})
	})
	if err != nil {
		return err
	}
	return os.WriteFile(path, mw.buf, 0o644)
}

func writeMinf(w *mp4.Writer, cfg event.CodecConfig) error {
	return box(w, mp4.BoxTypeMinf(), func() error {
		if err := leaf(w, mp4.BoxTypeSmhd(), &mp4.Smhd{}); err != nil {
			return err
		}
		if err := box(w, mp4.BoxTypeDinf(), func() error {
			if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDref()}); err != nil {
				return err
			}
			if _, err := mp4.Marshal(w, &mp4.Dref{EntryCount: 1}, mp4.Context{}); err != nil {
				return err
			}
			url := &mp4.Url{}
			url.SetFlags(mp4.UrlSelfContained)
			if err := leaf(w, mp4.BoxTypeUrl(), url); err != nil {
				return err
			}
			_, err := w.EndBox()
			return err
		}); err != nil {
			return err
		}
		return box(w, mp4.BoxTypeStbl(), func() error {
			if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsd()}); err != nil {
				return err
			}
			if _, err := mp4.Marshal(w, &mp4.Stsd{EntryCount: 1}, mp4.Context{}); err != nil {
				return err
			}
			if err := writeSampleEntry(w, cfg); err != nil {
				return err
			}
			if _, err := w.EndBox(); err != nil {
				return err
			}
			if err := leaf(w, mp4.BoxTypeStts(), &mp4.Stts{}); err != nil {
				return err
			}
			if err := leaf(w, mp4.BoxTypeStsc(), &mp4.Stsc{}); err != nil {
				return err
			}
			if err := leaf(w, mp4.BoxTypeStsz(), &mp4.Stsz{}); err != nil {
				return err
			}
			return leaf(w, mp4.BoxTypeStco(), &mp4.Stco{})
		})
	})
}

func writeSampleEntry(w *mp4.Writer, cfg event.CodecConfig) error {
	entry := &mp4.AudioSampleEntry{
		ChannelCount: uint16(cfg.Channels),
		SampleSize:   16,
		SampleRate:   cfg.ClockRate << 16,
	}
	entry.DataReferenceIndex = 1
	typ := mp4.BoxTypeMp4a()
	if cfg.Codec == event.CodecOpus {
		typ = mp4.BoxTypeOpus()
	}
	entry.SetType(typ)
	if _, err := w.StartBox(&mp4.BoxInfo{Type: typ}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, entry, mp4.Context{}); err != nil {
		return err
	}
	var err error
	if cfg.Codec == event.CodecOpus {
		err = leaf(w, mp4.BoxTypeDOps(), &mp4.DOps{
			OutputChannelCount: cfg.Channels,
			InputSampleRate:    cfg.ClockRate,
		})
	} else {
		err = leaf(w, mp4.BoxTypeEsds(), makeEsds(cfg))
	}
	if err != nil {
		return err
	}
	_, err = w.EndBox()
	return err
}

// aacFreqIndex maps sample rates to their AudioSpecificConfig index.
var aacFreqIndex = map[uint32]byte{
	96000: 0, 88200: 1, 64000: 2, 48000: 3, 44100: 4, 32000: 5,
	24000: 6, 22050: 7, 16000: 8, 12000: 9, 11025: 10, 8000: 11,
}

func makeEsds(cfg event.CodecConfig) *mp4.Esds {
	freqIdx, ok := aacFreqIndex[cfg.ClockRate]
	if !ok {
		freqIdx = 11
	}
	asc := []byte{2<<3 | freqIdx>>1, freqIdx<<7 | cfg.Channels<<3}
	return &mp4.Esds{Descriptors: []mp4.Descriptor{
		{Tag: mp4.ESDescrTag, Size: 3, ESDescriptor: &mp4.ESDescriptor{ESID: 1}},
		{Tag: mp4.DecoderConfigDescrTag, Size: 13, DecoderConfigDescriptor: &mp4.DecoderConfigDescriptor{
			ObjectTypeIndication: 0x40, StreamType: 5,
		}},
		{Tag: mp4.DecSpecificInfoTag, Size: uint32(len(asc)), Data: asc},
		{Tag: mp4.SLConfigDescrTag, Size: 1, Data: []byte{0x02}},
	}}
}

func writeSegment(dir string, n int, seqNum uint32, pkts []*event.Packet) (Info, error) {
	name := fmt.Sprintf("seg_%04d.m4s", n)
	path, err := safepath.Join(dir, name)
	if err != nil {
		return Info{}, err
	}
	start := pkts[0].MediaTime
	end := pkts[len(pkts)-1].MediaTime + pkts[len(pkts)-1].Duration
	trun := &mp4.Trun{SampleCount: uint32(len(pkts))}
	trun.SetFlags(0x000001 | 0x000100 | 0x000200) // data-offset, duration, size
	var data []byte
	for _, p := range pkts {
		trun.Entries = append(trun.Entries, mp4.TrunEntry{
			SampleDuration: uint32(p.Duration),
			SampleSize:     uint32(len(p.Payload)),
		})
		data = append(data, p.Payload...)
	}
	build := func(dataOffset int32) ([]byte, error) {
		trun.DataOffset = dataOffset
		mw := &memWS{}
		w := mp4.NewWriter(mw)
		err := box(w, mp4.BoxTypeMoof(), func() error {
			if err := leaf(w, mp4.BoxTypeMfhd(), &mp4.Mfhd{SequenceNumber: seqNum}); err != nil {
				return err
			}
			return box(w, mp4.BoxTypeTraf(), func() error {
				tfhd := &mp4.Tfhd{TrackID: 1}
				tfhd.SetFlags(mp4.TfhdDefaultBaseIsMoof)
				if err := leaf(w, mp4.BoxTypeTfhd(), tfhd); err != nil {
					return err
				}
				tfdt := &mp4.Tfdt{BaseMediaDecodeTimeV1: uint64(start)}
				tfdt.SetVersion(1)
				if err := leaf(w, mp4.BoxTypeTfdt(), tfdt); err != nil {
					return err
				}
				return leaf(w, mp4.BoxTypeTrun(), trun)
			})
		})
		if err != nil {
			return nil, err
		}
		return mw.buf, nil
	}
	moof, err := build(0)
	if err != nil {
		return Info{}, err
	}
	if moof, err = build(int32(len(moof) + 8)); err != nil { // + mdat header
		return Info{}, err
	}
	out := make([]byte, 0, len(moof)+8+len(data))
	out = append(out, moof...)
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(8+len(data)))
	copy(hdr[4:], "mdat")
	out = append(out, hdr[:]...)
	out = append(out, data...)
	if err := validate(out, len(pkts), start); err != nil {
		return Info{}, err
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return Info{}, err
	}
	sum := sha256.Sum256(out)
	return Info{
		Path: name, Start: start, End: end, StartSeq: pkts[0].SeqExt,
		Samples: len(pkts), Bytes: int64(len(out)), SHA256: hex.EncodeToString(sum[:]),
	}, nil
}

// validate reads the segment's box structure back and checks the decode
// time and sample count against what was written.
func validate(raw []byte, wantSamples int, wantStart clock.Time) error {
	var samples int
	var tfdtOK, trunOK bool
	_, err := mp4.ReadBoxStructure(bytes.NewReader(raw), func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case mp4.BoxTypeMoof(), mp4.BoxTypeTraf():
			return h.Expand()
		case mp4.BoxTypeTfdt():
			b, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			if got := b.(*mp4.Tfdt).GetBaseMediaDecodeTime(); got != uint64(wantStart) {
				return nil, fmt.Errorf("tfdt base time %d, want %d", got, wantStart)
			}
			tfdtOK = true
		case mp4.BoxTypeTrun():
			b, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			samples = int(b.(*mp4.Trun).SampleCount)
			trunOK = true
		}
		return nil, nil
	})
	if err != nil {
		return err
	}
	if !tfdtOK || !trunOK || samples != wantSamples {
		return &event.Error{Op: "segment.validate", Reason: event.ReasonTruncated,
			Err: fmt.Errorf("segment check failed: samples=%d want=%d tfdt=%v trun=%v",
				samples, wantSamples, tfdtOK, trunOK)}
	}
	return nil
}
