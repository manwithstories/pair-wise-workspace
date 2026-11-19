// Package mp4 demuxes MP4/fMP4 segments into per-track compressed samples
// using go-mp4, preserving track metadata (type, codec, timescale,
// language). It never decodes media. Truncated boxes and empty segments
// are reported with the byte offset of the failure.
package mp4

import (
	"bytes"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"archive/internal/errs"
)

// Sample is one compressed access unit. DTS is the raw decode time in the
// track timescale (for 32-bit clocks it may be wrapped); PTS is DTS +
// CTSOffset. Data is the untouched compressed payload.
type Sample struct {
	DTS       int64
	CTSOffset int32
	Duration  uint32
	Keyframe  bool
	Data      []byte
}

// Track is a demuxed elementary stream with its metadata.
type Track struct {
	ID        uint32
	Type      string // "video", "audio", "subtitle", "other"
	Codec     string // stsd entry type, e.g. "avc1", "mp4a", "wvtt"
	Timescale uint32
	Language  string // ISO-639-2/T, may be empty
	ClockBits int    // 32 for wrapped 32-bit clocks, 64 otherwise
	Samples   []Sample
}

// Demux parses one MP4/fMP4 segment and returns its tracks with samples.
func Demux(asset, name string, data []byte) ([]Track, error) {
	if len(data) == 0 {
		return nil, errs.New(errs.KindDemux, asset, name, "empty segment", nil)
	}
	d := &demuxer{asset: asset, name: name, data: data}
	r := bytes.NewReader(data)
	_, err := mp4.ReadBoxStructure(r, d.handle)
	if err != nil {
		if e, ok := err.(*errs.Error); ok {
			return nil, e
		}
		off, _ := r.Seek(0, io.SeekCurrent)
		return nil, errs.New(errs.KindDemux, asset,
			fmt.Sprintf("%s box offset %d", name, off), "malformed or truncated box", err)
	}
	if !d.sawMoov {
		return nil, errs.New(errs.KindDemux, asset, name, "no moov box found", nil)
	}
	if err := d.buildSamples(); err != nil {
		return nil, err
	}
	tracks := make([]Track, 0, len(d.tracks))
	for _, t := range d.tracks {
		tracks = append(tracks, *t)
	}
	return tracks, nil
}

// buildSamples is implemented in samples.go (fMP4) and stbl.go (plain MP4).
func (d *demuxer) buildSamples() error { return nil }

type trafState struct {
	trackID       uint32
	baseDTS       uint64
	clockBits     int
	baseDataOff   uint64
	hasBaseData   bool
	defDur        uint32
	defSize       uint32
	defFlags      uint32
	hasDefFlags   bool
	truns         []trunState
}

type trunState struct {
	dataOff   int32
	hasOff    bool
	firstFlgs uint32
	hasFirst  bool
	entries   []mp4.TrunEntry
}

type moofState struct {
	offset uint64
	trafs  []*trafState
}

type demuxer struct {
	asset, name string
	data        []byte
	tracks      []*Track
	byID        map[uint32]*Track
	cur         *Track
	curMoof     *moofState
	curTraf     *trafState
	moofs       []*moofState
	sawMoov     bool
}

func (d *demuxer) trackByID(id uint32) *Track {
	if d.byID == nil {
		d.byID = map[uint32]*Track{}
	}
	if t, ok := d.byID[id]; ok {
		return t
	}
	t := &Track{ID: id, Type: "other", ClockBits: 64}
	d.byID[id] = t
	d.tracks = append(d.tracks, t)
	return t
}

var containers = map[mp4.BoxType]bool{}

func init() {
	for _, t := range []mp4.BoxType{
		mp4.BoxTypeMoov(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(),
		mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), mp4.BoxTypeDinf(), mp4.BoxTypeEdts(),
	} {
		containers[t] = true
	}
}

func (d *demuxer) handle(h *mp4.ReadHandle) (interface{}, error) {
	typ := h.BoxInfo.Type
	path := h.Path
	parent := mp4.BoxTypeAny()
	if len(path) >= 2 {
		parent = path[len(path)-2]
	}
	switch typ {
	case mp4.BoxTypeMoov():
		d.sawMoov = true
		return h.Expand()
	case mp4.BoxTypeTrak():
		d.cur = &Track{Type: "other", ClockBits: 64}
		if _, err := h.Expand(); err != nil {
			return nil, err
		}
		// merge moov metadata into the canonical per-ID track (samples
		// may already have been attached from an earlier moof)
		meta := d.cur
		d.cur = nil
		t := d.trackByID(meta.ID)
		t.Type, t.Codec = meta.Type, meta.Codec
		t.Timescale, t.Language = meta.Timescale, meta.Language
		return nil, nil
	case mp4.BoxTypeMoof():
		d.curMoof = &moofState{offset: h.BoxInfo.Offset}
		if _, err := h.Expand(); err != nil {
			return nil, err
		}
		d.moofs = append(d.moofs, d.curMoof)
		d.curMoof = nil
		return nil, nil
	case mp4.BoxTypeTraf():
		d.curTraf = &trafState{clockBits: 64}
		if _, err := h.Expand(); err != nil {
			return nil, err
		}
		if d.curMoof != nil {
			d.curMoof.trafs = append(d.curMoof.trafs, d.curTraf)
		}
		d.curTraf = nil
		return nil, nil
	}
	if containers[typ] {
		return h.Expand()
	}
	if parent == mp4.BoxTypeStsd() && d.cur != nil && d.cur.Codec == "" {
		d.cur.Codec = typ.String()
		return nil, nil
	}
	payload, _, err := h.ReadPayload()
	if err != nil {
		return nil, errs.New(errs.KindDemux, d.asset,
			fmt.Sprintf("%s box offset %d", d.name, h.BoxInfo.Offset), "cannot parse box", err)
	}
	switch b := payload.(type) {
	case *mp4.Tkhd:
		if d.cur != nil {
			d.cur.ID = b.TrackID
		}
	case *mp4.Mdhd:
		if d.cur != nil {
			d.cur.Timescale = b.Timescale
			d.cur.Language = langString(b.Language)
		}
	case *mp4.Hdlr:
		if d.cur != nil {
			d.cur.Type = handlerType(b.HandlerType)
		}
	case *mp4.Tfhd:
		if d.curTraf != nil {
			t := d.curTraf
			t.trackID = b.TrackID
			if flags32(b.FullBox)&0x000001 != 0 {
				t.baseDataOff, t.hasBaseData = b.BaseDataOffset, true
			}
			if flags32(b.FullBox)&0x000008 != 0 {
				t.defDur = b.DefaultSampleDuration
			}
			if flags32(b.FullBox)&0x000010 != 0 {
				t.defSize = b.DefaultSampleSize
			}
			if flags32(b.FullBox)&0x000020 != 0 {
				t.defFlags, t.hasDefFlags = b.DefaultSampleFlags, true
			}
		}
	case *mp4.Tfdt:
		if d.curTraf != nil {
			if b.FullBox.Version == 1 {
				d.curTraf.baseDTS, d.curTraf.clockBits = b.BaseMediaDecodeTimeV1, 64
			} else {
				d.curTraf.baseDTS, d.curTraf.clockBits = uint64(b.BaseMediaDecodeTimeV0), 32
			}
		}
	case *mp4.Trun:
		if d.curTraf != nil {
			tr := trunState{entries: b.Entries}
			if flags32(b.FullBox)&0x000001 != 0 {
				tr.dataOff, tr.hasOff = b.DataOffset, true
			}
			if flags32(b.FullBox)&0x000004 != 0 {
				tr.firstFlgs, tr.hasFirst = b.FirstSampleFlags, true
			}
			d.curTraf.truns = append(d.curTraf.truns, tr)
		}
	}
	return nil, nil
}

// flags32 packs a FullBox 24-bit flags field into a uint32.
func flags32(f mp4.FullBox) uint32 {
	return uint32(f.Flags[0])<<16 | uint32(f.Flags[1])<<8 | uint32(f.Flags[2])
}

func langString(l [3]byte) string {
	s := make([]byte, 0, 3)
	for _, c := range l {
		if c >= 'a' && c <= 'z' {
			s = append(s, c)
		}
	}
	return string(s)
}

func handlerType(h [4]byte) string {
	switch string(h[:]) {
	case "vide":
		return "video"
	case "soun":
		return "audio"
	case "subt", "sbtl", "text", "clcp":
		return "subtitle"
	}
	return "other"
}
