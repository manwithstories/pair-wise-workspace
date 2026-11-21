package metadata

import (
	"fmt"
	"io"
	"os"

	"github.com/abema/go-mp4"

	"mediamerge/internal/clock"
	"mediamerge/internal/fault"
)

// mp4Parser scans a file's box tree once, collecting the boxes the pipeline
// needs. It validates every box boundary itself so a corrupt length is
// reported with the owning offset rather than as an opaque library error.
type mp4Parser struct {
	file      *os.File
	size      int64
	container *ColorDesc
	diagn     fault.Report
	tracks    []*Track
}

// rawTrack accumulates per-track boxes found during one traversal.
type rawTrack struct {
	id       int
	kind     Kind
	tb       clock.Timebase
	duration int64
	width    int
	height   int
	matrix   [9]int32
	crop     *Crop
	color    ColorDesc
	rate     int
	channels int
	deltas   []int64
	keys     []int
}

// ParseMP4 reads path and returns canonical tracks. Corruption is reported as
// a fault with the byte offset of the offending box.
func ParseMP4(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fault.Wrap(fault.StageParse, fault.CodeParseBox, err, path)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fault.Wrap(fault.StageParse, fault.CodeParseBox, err, path)
	}
	p := &mp4Parser{file: f, size: st.Size()}
	file := &File{Container: "mp4"}
	if err := p.scan(0, p.size, nil); err != nil {
		return nil, err
	}
	file.Container = "mp4"
	file.Color = p.container
	file.Tracks = p.tracks
	file.Diagnostics = p.diagn
	// A missing container descriptor is expected (MP4 may carry colour only
	// per track); it surfaces through colour resolution, not here.
	return file, nil
}

// scan walks consecutive boxes in [start,end) and dispatches known containers.
func (p *mp4Parser) scan(start, end int64, raw *rawTrack) error {
	off := start
	for off < end {
		if end-off < 8 {
			return p.corrupt(off, "trailing %d bytes cannot hold a box header", end-off)
		}
		var hdr [8]byte
		if _, err := p.file.ReadAt(hdr[:], off); err != nil {
			return fault.Wrap(fault.StageParse, fault.CodeTruncatedBox, err, fmt.Sprintf("header at %d", off))
		}
		size := int64(uint32(hdr[0])<<24 | uint32(hdr[1])<<16 | uint32(hdr[2])<<8 | uint32(hdr[3]))
		typ := string(hdr[4:8])
		headerSize := int64(8)
		if size == 1 {
			var ext [8]byte
			if _, err := p.file.ReadAt(ext[:], off+8); err != nil {
				return fault.Wrap(fault.StageParse, fault.CodeTruncatedBox, err, fmt.Sprintf("extended size at %d", off))
			}
			size = int64(uint64(ext[0])<<56 | uint64(ext[1])<<48 | uint64(ext[2])<<40 | uint64(ext[3])<<32 |
				uint64(ext[4])<<24 | uint64(ext[5])<<16 | uint64(ext[6])<<8 | uint64(ext[7]))
			headerSize = 16
		} else if size == 0 {
			size = end - off
		}
		if size < headerSize {
			return p.corrupt(off, "box %q declares length %d smaller than its %d-byte header", typ, size, headerSize)
		}
		if off+size > end {
			return p.corruptBox(typ, off, size, end)
		}

		payload := off + headerSize
		switch typ {
		case "moov":
			if err := p.scan(payload, off+size, raw); err != nil {
				return err
			}
		case "trak":
			rt := &rawTrack{}
			if err := p.scan(payload, off+size, rt); err != nil {
				return err
			}
			if err := p.addTrack(rt); err != nil {
				return err
			}
		case "mdia", "minf", "stbl":
			if err := p.scan(payload, off+size, raw); err != nil {
				return err
			}
		case "stsd":
			// FullBox header plus entry_count, then sample entries.
			if payload+8 > off+size {
				return p.corruptBox(typ, off, size, end)
			}
			if err := p.scan(payload+8, off+size, raw); err != nil {
				return err
			}
		case "ccol":
			if err := p.readCCol(payload, off+size); err != nil {
				return err
			}
		default:
			if raw != nil {
				if err := p.readTrackBox(typ, payload, off+size, size-headerSize, raw); err != nil {
					return err
				}
			}
		}
		off += size
	}
	return nil
}

func (p *mp4Parser) corrupt(off int64, format string, args ...interface{}) error {
	return fault.New(fault.StageParse, fault.CodeTruncatedBox,
		fmt.Sprintf("mp4 box at offset %d: ", off)+fmt.Sprintf(format, args...))
}

func (p *mp4Parser) corruptBox(typ string, off, size, end int64) error {
	return fault.New(fault.StageParse, fault.CodeTruncatedBox,
		fmt.Sprintf("mp4 box %q at offset %d declares %d bytes but enclosing range ends at %d", typ, off, size, end))
}

// readPayload unmarshals a box payload into dst using the library marshaler.
func (p *mp4Parser) readPayload(payload, end int64, dst mp4.IBox) error {
	if end < payload {
		return fmt.Errorf("metadata: negative payload")
	}
	if _, err := p.file.Seek(payload, io.SeekStart); err != nil {
		return err
	}
	_, err := mp4.Unmarshal(p.file, uint64(end-payload), dst, mp4.Context{})
	return err
}

func (p *mp4Parser) readCCol(payload, end int64) error {
	var b CColBox
	if err := p.readPayload(payload, end, &b); err != nil {
		return fault.Wrap(fault.StageParse, fault.CodeParseBox, err, "ccol")
	}
	d := b.Descriptor()
	p.container = &d
	return nil
}

func (p *mp4Parser) readTrackBox(typ string, payload, end, payloadSize int64, raw *rawTrack) error {
	switch typ {
	case "tkhd":
		var b mp4.Tkhd
		if err := p.readPayload(payload, end, &b); err != nil {
			return fault.Wrap(fault.StageParse, fault.CodeParseBox, err, "tkhd")
		}
		raw.id = int(b.TrackID)
		raw.width = int(b.Width >> 16)
		raw.height = int(b.Height >> 16)
		raw.matrix = b.Matrix
	case "mdhd":
		var b mp4.Mdhd
		if err := p.readPayload(payload, end, &b); err != nil {
			return fault.Wrap(fault.StageParse, fault.CodeParseBox, err, "mdhd")
		}
		if b.Timescale == 0 {
			return fault.NewTrack(fault.StageParse, fault.CodeParseBox, raw.id, "mdhd timescale is zero")
		}
		raw.tb = retimebase(uint32(b.Timescale))
		raw.duration = int64(b.GetDuration())
	case "hdlr":
		var b mp4.Hdlr
		if err := p.readPayload(payload, end, &b); err != nil {
			return fault.Wrap(fault.StageParse, fault.CodeParseBox, err, "hdlr")
		}
		switch string(b.HandlerType[:]) {
		case "vide":
			raw.kind = KindVideo
		case "soun":
			raw.kind = KindAudio
		}
	case "avc1", "mp4a":
		return p.readSampleEntry(typ, payload, end, raw)
	case "stts":
		var b mp4.Stts
		if err := p.readPayload(payload, end, &b); err != nil {
			return fault.Wrap(fault.StageParse, fault.CodeParseBox, err, "stts")
		}
		if b.EntryCount == 0 {
			return fault.NewTrack(fault.StageParse, fault.CodeParseBox, raw.id, "stts has no entries")
		}
		total := 0
		for _, e := range b.Entries {
			total += int(e.SampleCount)
			for i := uint32(0); i < e.SampleCount; i++ {
				raw.deltas = append(raw.deltas, int64(e.SampleDelta))
			}
		}
		if total == 0 {
			return fault.NewTrack(fault.StageParse, fault.CodeParseBox, raw.id, "stts declares zero samples")
		}
	case "stss":
		var b mp4.Stss
		if err := p.readPayload(payload, end, &b); err != nil {
			return fault.Wrap(fault.StageParse, fault.CodeParseBox, err, "stss")
		}
		for _, n := range b.SampleNumber {
			raw.keys = append(raw.keys, int(n))
		}
	}
	return nil
}

// readSampleEntry parses the fixed fields, then walks child boxes (colr,
// clap) that follow the entry header.
func (p *mp4Parser) readSampleEntry(typ string, payload, end int64, raw *rawTrack) error {
	if typ == "avc1" {
		var b mp4.VisualSampleEntry
		b.Type = mp4.StrToBoxType("avc1")
		if err := p.readPayload(payload, end, &b); err != nil {
			return fault.Wrap(fault.StageParse, fault.CodeParseBox, err, "avc1")
		}
		raw.width = int(b.Width)
		raw.height = int(b.Height)
	} else {
		var b mp4.AudioSampleEntry
		b.Type = mp4.StrToBoxType("mp4a")
		if err := p.readPayload(payload, end, &b); err != nil {
			return fault.Wrap(fault.StageParse, fault.CodeParseBox, err, "mp4a")
		}
		raw.channels = int(b.ChannelCount)
		raw.rate = int(b.SampleRate >> 16)
		raw.kind = KindAudio
	}
	// Child boxes start after the fixed entry structure plus the
	// 4-byte EntryCount already consumed by stsd's child walk. The scan
	// below locates them by signature because the entry payload contains
	// no child count.
	return p.scanChildren(payload, end, raw)
}

// scanChildren finds known extension boxes by walking them from the end of
// the entry header. Only colr and clap are relevant to the canonical model.
func (p *mp4Parser) scanChildren(payload, end int64, raw *rawTrack) error {
	// Visual entry: 8 (box hdr already excluded) ... determine by probing
	// for the 4-char signatures at 8-byte granularity is unsafe; instead
	// the writer places children immediately after a known-sized entry.
	childStart := payload + entryHeaderLen(raw.kind)
	for childStart+8 <= end {
		var hdr [8]byte
		if _, err := p.file.ReadAt(hdr[:], childStart); err != nil {
			return fault.Wrap(fault.StageParse, fault.CodeTruncatedBox, err, "sample entry child")
		}
		size := int64(uint32(hdr[0])<<24 | uint32(hdr[1])<<16 | uint32(hdr[2])<<8 | uint32(hdr[3]))
		typ := string(hdr[4:8])
		if size < 8 || childStart+size > end {
			return p.corruptBox(typ, childStart, size, end)
		}
		switch typ {
		case "colr":
			var b mp4.Colr
			if err := p.readPayload(childStart+8, childStart+size, &b); err != nil {
				return fault.Wrap(fault.StageParse, fault.CodeParseBox, err, "colr")
			}
			raw.color = colrDescriptor(&b)
		case "clap":
			var b ClapBox
			if err := p.readPayload(childStart+8, childStart+size, &b); err != nil {
				return fault.Wrap(fault.StageParse, fault.CodeParseBox, err, "clap")
			}
			c, err := b.Crop(raw.id)
			if err != nil {
				return fault.NewTrack(fault.StageParse, fault.CodeCropOutOfBounds, raw.id, err.Error())
			}
			raw.crop = &c
		}
		childStart += size
	}
	return nil
}

// entryHeaderLen is the number of payload bytes before child boxes: the
// 6 reserved bytes plus data_reference_index, then visual/audio specifics.
func entryHeaderLen(kind Kind) int64 {
	if kind == KindAudio {
		return 28
	}
	return 78
}

func colrDescriptor(b *mp4.Colr) ColorDesc {
	if b.ColourType != [4]byte{'n', 'c', 'l', 'x'} {
		return ColorDesc{}
	}
	return NewColorDesc(b.ColourPrimaries, b.TransferCharacteristics, b.MatrixCoefficients, &b.FullRangeFlag, "track")
}

func (p *mp4Parser) addTrack(raw *rawTrack) error {
	if raw.kind == "" {
		// Non-media tracks (e.g. metadata) are intentionally skipped.
		return nil
	}
	if raw.tb.Rate.Num == 0 {
		return fault.NewTrack(fault.StageParse, fault.CodeParseBox, raw.id, "track has no mdhd")
	}
	tr := &Track{
		ID: raw.id, Kind: raw.kind, Timebase: raw.tb, DurationTicks: raw.duration,
		Width: raw.width, Height: raw.height, Crop: raw.crop, Color: raw.color,
		SampleRate: raw.rate, Channels: raw.channels,
	}
	if raw.kind == KindVideo {
		span := tr.Span()
		rot, err := RotationForMatrix(raw.id, raw.matrix, span)
		if err != nil {
			return err
		}
		tr.Rotation = rot
	}
	if len(raw.deltas) > 0 {
		idx, err := BuildKeyframeIndex(raw.deltas, raw.keys, raw.tb)
		if err != nil {
			return fault.NewTrack(fault.StageParse, fault.CodeParseBox, raw.id, err.Error())
		}
		tr.Keyframes = idx
	}
	p.tracks = append(p.tracks, tr)
	return nil
}

// retimebase turns an integer timescale into a rational rate.
func retimebase(ts uint32) clock.Timebase {
	return clock.Must(int64(ts), 1)
}
