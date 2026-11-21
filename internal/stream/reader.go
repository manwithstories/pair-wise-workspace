// Package stream validates fragmented MP4 segments and RTP captures without
// loading whole media files into memory.
package stream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	mp4 "github.com/abema/go-mp4"

	"mediascan/internal/model"
)

// Hard limits that keep a hostile or truncated file from exhausting memory.
const (
	MaxBoxBytes    = 32 << 20
	MaxMoofBytes   = 8 << 20
	MaxSamples     = 200_000
	MinBoxHeader   = 8
	LargeBoxHeader = 16
)

// TrackMeta describes a track declared by a box hierarchy.
type TrackMeta struct {
	TrackID   uint32
	Timescale uint32
	Kind      string // "video" | "audio" | "unknown"
	Width     uint32
	Height    uint32
}

// Init maps track IDs to their declared timescale and kind.
type Init struct {
	Path   string
	Tracks map[uint32]TrackMeta
}

// Get returns the metadata for a track, or an "unknown" entry.
func (i *Init) Get(trackID uint32) TrackMeta {
	if i != nil {
		if meta, ok := i.Tracks[trackID]; ok {
			return meta
		}
	}
	return TrackMeta{TrackID: trackID, Kind: "unknown"}
}

// Sample is one media sample extracted from a trun.
type Sample struct {
	TrackID  uint32
	Decode   uint64 // in the track timescale
	Duration uint32
	Size     uint32
	Sync     bool
	CTO      int64 // composition time offset, track ticks
}

// Traf is one track fragment inside a moof.
type Traf struct {
	TrackID  uint32
	BaseTime uint64
	Samples  []Sample
}

// SegmentScan is the validated view of one media segment.
type SegmentScan struct {
	Name      string
	Path      string
	ByteStart int64
	ByteEnd   int64
	Boxes     int
	Trafs     []Traf
	Faults    []model.Fault
}

// FirstUndecodable returns the first box-structure fault, if any.
func (s *SegmentScan) FirstUndecodable() (model.Fault, bool) {
	for _, f := range s.Faults {
		switch f.Kind {
		case model.FaultTruncatedBox, model.FaultBoxBounds, model.FaultBoxTooLarge:
			return f, true
		}
	}
	return model.Fault{}, false
}

// LoadInit parses the moov box of an initialization segment. Track IDs are
// registered from tkhd; mdhd/hdlr that follow in the same trak refine the
// entry. go-mp4 descends children directly so this stays a single pass.
func LoadInit(path string) (*Init, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st := &initWalker{info: &Init{Path: path, Tracks: map[uint32]TrackMeta{}}}
	_, err = mp4.ReadBoxStructure(f, st.handle)
	if err != nil {
		return nil, err
	}
	if len(st.info.Tracks) == 0 {
		return nil, fmt.Errorf("%w: no tracks in %s", model.ErrBoxBounds, path)
	}
	return st.info, nil
}

type initWalker struct {
	info     *Init
	lastTrak uint32
}

func (w *initWalker) handle(h *mp4.ReadHandle) (interface{}, error) {
	switch h.BoxInfo.Type {
	case mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia():
		_, err := h.Expand()
		return nil, err
	case mp4.BoxTypeTkhd():
		box, _, err := h.ReadPayload()
		if err != nil {
			return nil, err
		}
		tk := box.(*mp4.Tkhd)
		meta := w.info.Tracks[tk.TrackID]
		meta.TrackID = tk.TrackID
		meta.Width = tk.Width >> 16
		meta.Height = tk.Height >> 16
		w.info.Tracks[tk.TrackID] = meta
		w.lastTrak = tk.TrackID
	case mp4.BoxTypeMdhd():
		box, _, err := h.ReadPayload()
		if err != nil {
			return nil, err
		}
		mdhd := box.(*mp4.Mdhd)
		if err := model.CheckTimescale(mdhd.Timescale); err != nil {
			return nil, fmt.Errorf("track %d: %w", w.lastTrak, err)
		}
		meta := w.info.Tracks[w.lastTrak]
		meta.TrackID = w.lastTrak
		meta.Timescale = mdhd.Timescale
		w.info.Tracks[w.lastTrak] = meta
	case mp4.BoxTypeHdlr():
		box, _, err := h.ReadPayload()
		if err != nil {
			return nil, err
		}
		hdlr := box.(*mp4.Hdlr)
		kind := "unknown"
		switch string(hdlr.HandlerType[:]) {
		case "vide":
			kind = "video"
		case "soun":
			kind = "audio"
		}
		meta := w.info.Tracks[w.lastTrak]
		meta.TrackID = w.lastTrak
		meta.Kind = kind
		w.info.Tracks[w.lastTrak] = meta
	}
	return nil, nil
}

// readBoxHeaderAt reads one box header and validates it against the bytes
// remaining in the enclosing region.
func readBoxHeaderAt(r io.ReadSeeker, remaining int64) (*mp4.BoxInfo, error) {
	if remaining < MinBoxHeader {
		return nil, fmt.Errorf("%w: %d trailing bytes cannot hold a box header", model.ErrTruncated, remaining)
	}
	start, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	bi, err := mp4.ReadBoxInfo(r)
	if err != nil {
		return nil, fmt.Errorf("%w: short box header at %d: %v", model.ErrTruncated, start, err)
	}
	if bi.Size < bi.HeaderSize {
		return nil, fmt.Errorf("%w: box %s declares %d < header %d",
			model.ErrBoxBounds, bi.Type.String(), bi.Size, bi.HeaderSize)
	}
	if bi.Size > uint64(remaining) {
		return nil, fmt.Errorf("%w: box %s declares %d bytes, %d remain",
			model.ErrTruncated, bi.Type.String(), bi.Size, remaining)
	}
	if bi.Size > MaxBoxBytes {
		return nil, fmt.Errorf("%w: box %s declares %d bytes",
			model.ErrBoxTooLarge, bi.Type.String(), bi.Size)
	}
	return bi, nil
}

// validateMoof performs a bounds-first walk over a moof payload so go-mp4 is
// never asked to expand a trun declaring more samples than its bytes hold.
func validateMoof(payload []byte) error {
	return walkChildren(payload, "moof", func(typ string, body []byte) error {
		if typ == "traf" {
			return walkChildren(body, "traf", func(ctyp string, cbody []byte) error {
				if ctyp == "trun" {
					return validateTrun(cbody)
				}
				return nil
			})
		}
		return nil
	})
}

func walkChildren(payload []byte, parent string, fn func(typ string, body []byte) error) error {
	for off := 0; off < len(payload); {
		if len(payload)-off < MinBoxHeader {
			return fmt.Errorf("%w: %s has %d trailing bytes", model.ErrTruncated, parent, len(payload)-off)
		}
		size := binary.BigEndian.Uint32(payload[off:])
		typ := string(payload[off+4 : off+8])
		hdr := uint32(MinBoxHeader)
		switch size {
		case 0:
			size = uint32(len(payload) - off)
		case 1:
			if len(payload)-off < LargeBoxHeader {
				return fmt.Errorf("%w: %s largesize header", model.ErrTruncated, typ)
			}
			large := binary.BigEndian.Uint64(payload[off+8:])
			if large < LargeBoxHeader || large > MaxBoxBytes {
				return fmt.Errorf("%w: %s largesize %d", model.ErrBoxBounds, typ, large)
			}
			size = uint32(large)
			hdr = LargeBoxHeader
		}
		if size < hdr {
			return fmt.Errorf("%w: %s child %s declares size %d below header %d",
				model.ErrBoxBounds, parent, typ, size, hdr)
		}
		if uint64(off)+uint64(size) > uint64(len(payload)) {
			return fmt.Errorf("%w: %s child %s size %d exceeds payload", model.ErrTruncated, parent, typ, size)
		}
		if err := fn(typ, payload[off+int(hdr):off+int(size)]); err != nil {
			return err
		}
		off += int(size)
	}
	return nil
}

func validateTrun(payload []byte) error {
	if len(payload) < 8 {
		return fmt.Errorf("%w: trun payload %d bytes", model.ErrTruncated, len(payload))
	}
	flags := binary.BigEndian.Uint32(payload[0:]) & 0xFFFFFF
	count := binary.BigEndian.Uint32(payload[4:])
	entry := 0
	for _, bit := range []uint32{0x100, 0x200, 0x400, 0x800} {
		if flags&bit != 0 {
			entry += 4
		}
	}
	need := int64(0)
	if flags&0x1 != 0 {
		need += 4
	}
	if flags&0x4 != 0 {
		need += 4
	}
	if int64(count)*int64(entry)+need > int64(len(payload)-8) {
		return fmt.Errorf("%w: trun declares %d samples of %d bytes in %d-byte payload",
			model.ErrBoxBounds, count, entry, len(payload))
	}
	if count > MaxSamples {
		return fmt.Errorf("%w: trun sample count %d", model.ErrBoxTooLarge, count)
	}
	return nil
}

// ScanSegment validates and decodes one media segment, honouring an optional
// byte range (ByteEnd < 0 means the whole file). Only a moof payload is
// buffered; mdat and other payloads are skipped by seeking.
func ScanSegment(init *Init, name, path string, byteStart, byteEnd int64) (*SegmentScan, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	region := size
	if byteEnd >= 0 {
		if byteStart < 0 || byteEnd < byteStart || byteEnd >= size {
			return nil, fmt.Errorf("%w: range %d-%d of %d bytes", model.ErrBoxBounds, byteStart, byteEnd, size)
		}
		region = byteEnd - byteStart + 1
		byteEnd = byteStart + region - 1
	} else {
		byteStart, byteEnd = 0, size-1
	}
	scan := &SegmentScan{Name: name, Path: path, ByteStart: byteStart, ByteEnd: byteEnd}
	var r io.ReadSeeker = f
	if region != size {
		r = io.NewSectionReader(f, byteStart, region)
	}
	var offset int64
	for offset < region {
		bi, hdrErr := readBoxHeaderAt(r, region-offset)
		if hdrErr != nil {
			scan.Faults = append(scan.Faults, headerFault(scan, hdrErr, offset, region))
			break
		}
		scan.Boxes++
		payloadLen := int64(bi.Size - bi.HeaderSize)
		if bi.Type == mp4.BoxTypeMoof() {
			payload := make([]byte, payloadLen)
			if _, err := io.ReadFull(r, payload); err != nil {
				scan.Faults = append(scan.Faults, spanFault(scan, model.FaultTruncatedBox,
					"moof payload truncated", offset, int64(bi.Size)))
				break
			}
			if err := validateMoof(payload); err != nil {
				scan.Faults = append(scan.Faults, spanFault(scan, boxFaultKind(err), err.Error(), offset, int64(bi.Size)))
				break
			}
			trafs, err := parseMoof(payload)
			if err != nil {
				scan.Faults = append(scan.Faults, spanFault(scan, model.FaultBoxBounds,
					"moof decode: "+err.Error(), offset, int64(bi.Size)))
				break
			}
			scan.Trafs = append(scan.Trafs, trafs...)
			offset += int64(bi.Size)
			continue
		}
		// ReadBoxInfo leaves the reader at the payload start; skip only the
		// payload bytes to land on the next box header.
		if _, err := r.Seek(int64(payloadLen), io.SeekCurrent); err != nil {
			return scan, err
		}
		offset += int64(bi.Size)
	}
	return scan, nil
}

func boxFaultKind(err error) model.FaultKind {
	switch {
	case errors.Is(err, model.ErrTruncated):
		return model.FaultTruncatedBox
	case errors.Is(err, model.ErrBoxTooLarge):
		return model.FaultBoxTooLarge
	default:
		return model.FaultBoxBounds
	}
}

func headerFault(scan *SegmentScan, err error, offset, region int64) model.Fault {
	return spanFault(scan, boxFaultKind(err), err.Error(), offset, region-offset)
}

func spanFault(scan *SegmentScan, kind model.FaultKind, msg string, offset, length int64) model.Fault {
	start := scan.ByteStart + offset
	end := start + length - 1
	if end > scan.ByteEnd {
		end = scan.ByteEnd
	}
	return model.Fault{Kind: kind, Severity: model.SeverityCritical, Segment: scan.Name,
		Message: msg, ByteStart: start, ByteEnd: end}
}

// parseMoof decodes traf boxes into samples once the payload is known safe.
func parseMoof(payload []byte) ([]Traf, error) {
	var out []Traf
	w := &moofWalker{out: &out}
	if _, err := mp4.ReadBoxStructure(bytes.NewReader(payload), w.handle); err != nil {
		return nil, err
	}
	return out, nil
}

type moofWalker struct {
	out  *[]Traf
	id   uint32
	base uint64
}

func (w *moofWalker) handle(h *mp4.ReadHandle) (interface{}, error) {
	switch h.BoxInfo.Type {
	case mp4.BoxTypeTraf():
		*w.out = append(*w.out, Traf{})
		_, err := h.Expand()
		return nil, err
	case mp4.BoxTypeTfhd():
		box, _, err := h.ReadPayload()
		if err != nil {
			return nil, err
		}
		w.id = box.(*mp4.Tfhd).TrackID
	case mp4.BoxTypeTfdt():
		box, _, err := h.ReadPayload()
		if err != nil {
			return nil, err
		}
		w.base = box.(*mp4.Tfdt).GetBaseMediaDecodeTime()
	case mp4.BoxTypeTrun():
		box, _, err := h.ReadPayload()
		if err != nil {
			return nil, err
		}
		trun := box.(*mp4.Trun)
		traf := &(*w.out)[len(*w.out)-1]
		traf.TrackID = w.id
		traf.BaseTime = w.base
		time := w.base
		for i := 0; i < int(trun.SampleCount) && i < len(trun.Entries); i++ {
			e := trun.Entries[i]
			s := Sample{TrackID: w.id, Decode: time, Duration: e.SampleDuration,
				Size: e.SampleSize, Sync: e.SampleFlags&0x00010000 == 0, CTO: trun.GetSampleCompositionTimeOffset(i)}
			traf.Samples = append(traf.Samples, s)
			time += uint64(e.SampleDuration)
		}
	}
	return nil, nil
}
