// Package probe reads MP4/MOV containers with go-mp4, builds per-track
// sample timelines on the unified clock, and localizes the first
// damaged box to a byte offset and a media time (within one video
// frame). Unknown codec structures fail explicitly.
package probe

import (
	"errors"
	"fmt"
	"os"

	mp4 "github.com/abema/go-mp4"

	"mediac/internal/clock"
	"mediac/internal/model"
)

// MaxFileSize is the hard input limit (20 GB).
const MaxFileSize = 20 << 30

// Sample is one access unit on the track timeline.
type Sample struct {
	Index        int
	DTS, PTS     int64 // track timescale units
	Duration     int64
	Offset, Size int64 // file byte range
	Keyframe     bool
}

// ColorMeta carries nclx colour metadata of a video track.
type ColorMeta struct {
	Primaries, Transfer, Matrix uint16
	FullRange                   bool
}

// Track is a parsed elementary track with its sample timeline.
type Track struct {
	ID         uint32
	Kind       string // "video" | "audio"
	Codec      string
	Timescale  int64
	Channels   int
	SampleRate int
	Color      *ColorMeta
	Samples    []Sample
}

// Duration returns the exact track duration on the unified clock.
func (t *Track) Duration() clock.Rational {
	var d int64
	for _, s := range t.Samples {
		d += s.Duration
	}
	return clock.New(d, t.Timescale)
}

// Damage localizes the first damaged box.
type Damage struct {
	BoxType     string
	OffsetBytes int64
	MediaTime   clock.Rational
}

// Result is the outcome of probing one file.
type Result struct {
	Path   string
	Size   int64
	Tracks []Track
	Damage *Damage
}

// Track returns the first track of the given kind, if any.
func (r *Result) Track(kind string) *Track {
	for i := range r.Tracks {
		if r.Tracks[i].Kind == kind {
			return &r.Tracks[i]
		}
	}
	return nil
}

var errStop = errors.New("probe: stop walk")

type trackBuilder struct {
	track                Track
	stts                 *mp4.Stts
	ctts                 *mp4.Ctts
	stsc                 *mp4.Stsc
	stsz                 *mp4.Stsz
	stco                 *mp4.Stco
	stss                 *mp4.Stss
}

type parser struct {
	size     int64
	cur      *trackBuilder
	tracks   []*trackBuilder
	mdatLo   int64
	mdatHi   int64
	knownDur clock.Rational // longest duration whose mdhd parsed
	damage   *Damage
}

// Probe parses one MP4/MOV file. Structural damage is reported in
// Result.Damage and as a *model.MediaError with exit code 2.
func Probe(path string) (*Result, *model.MediaError) {
	f, err := os.Open(path)
	if err != nil {
		return nil, model.New(model.StageProbe, "open_failed", err.Error(), model.ExitDamagedInput)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, model.New(model.StageProbe, "stat_failed", err.Error(), model.ExitDamagedInput)
	}
	if st.Size() > MaxFileSize {
		return nil, model.New(model.StageProbe, "too_large",
			fmt.Sprintf("file %s exceeds 20 GB limit", path), model.ExitDamagedInput)
	}
	p := &parser{size: st.Size(), knownDur: clock.New(0, 1)}
	_, walkErr := mp4.ReadBoxStructure(f, p.handle)
	if walkErr != nil && !errors.Is(walkErr, errStop) {
		if me, ok := walkErr.(*model.MediaError); ok {
			return nil, me
		}
		return nil, model.New(model.StageProbe, "parse_failed", walkErr.Error(), model.ExitDamagedInput)
	}
	res := &Result{Path: path, Size: st.Size(), Damage: p.damage}
	for _, b := range p.tracks {
		if merr := b.build(); merr != nil {
			return nil, merr
		}
		res.Tracks = append(res.Tracks, b.track)
	}
	if len(res.Tracks) == 0 && res.Damage == nil {
		return nil, model.New(model.StageProbe, "no_moov",
			"no parseable tracks: missing or truncated moov", model.ExitDamagedInput)
	}
	if res.Damage == nil {
		res.Damage = p.validate()
	}
	if res.Damage != nil {
		me := model.New(model.StageProbe, "damaged_input",
			fmt.Sprintf("first damaged box %s at byte %d", res.Damage.BoxType, res.Damage.OffsetBytes),
			model.ExitDamagedInput)
		me.OffsetBytes = res.Damage.OffsetBytes
		me.MediaTimeMs = res.Damage.MediaTime.MillisFloor()
		return res, me
	}
	return res, nil
}

func (p *parser) handle(h *mp4.ReadHandle) (interface{}, error) {
	bi := &h.BoxInfo
	if int64(bi.Offset+bi.Size) > p.size { // box extends past EOF: truncated
		switch bi.Type { // containers: parse the surviving children first
		case mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(),
			mp4.BoxTypeMinf(), mp4.BoxTypeStbl():
			if _, err := h.Expand(); err != nil && p.damage == nil {
				p.damage = &Damage{BoxType: bi.Type.String(), OffsetBytes: p.size, MediaTime: p.knownDur}
			}
			return nil, errStop
		}
		p.damage = &Damage{BoxType: bi.Type.String(), OffsetBytes: p.size, MediaTime: p.knownDur}
		return nil, errStop
	}
	switch bi.Type {
	case mp4.BoxTypeMoov(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(),
		mp4.BoxTypeStbl(), mp4.BoxTypeDinf(), mp4.BoxTypeStsd():
		return h.Expand()
	case mp4.BoxTypeTrak():
		p.cur = &trackBuilder{}
		p.tracks = append(p.tracks, p.cur)
		return h.Expand()
	case mp4.BoxTypeMdat():
		p.mdatLo = int64(bi.Offset + uint64(bi.HeaderSize))
		p.mdatHi = int64(bi.Offset + bi.Size)
	case mp4.BoxTypeTkhd():
		if box, _, err := h.ReadPayload(); err == nil {
			p.cur.track.ID = box.(*mp4.Tkhd).TrackID
		}
	case mp4.BoxTypeMdhd():
		if box, _, err := h.ReadPayload(); err == nil {
			m := box.(*mp4.Mdhd)
			p.cur.track.Timescale = int64(m.Timescale)
			if d := clock.New(int64(m.DurationV0), int64(m.Timescale)); d.Cmp(p.knownDur) > 0 {
				p.knownDur = d
			}
		}
	case mp4.BoxTypeHdlr():
		if box, _, err := h.ReadPayload(); err == nil {
			switch string(box.(*mp4.Hdlr).HandlerType[:]) {
			case "vide":
				p.cur.track.Kind = "video"
			case "soun":
				p.cur.track.Kind = "audio"
			}
		}
	case mp4.BoxTypeStts():
		if box, _, err := h.ReadPayload(); err == nil {
			p.cur.stts = box.(*mp4.Stts)
		}
	case mp4.BoxTypeCtts():
		if box, _, err := h.ReadPayload(); err == nil {
			p.cur.ctts = box.(*mp4.Ctts)
		}
	case mp4.BoxTypeStsc():
		if box, _, err := h.ReadPayload(); err == nil {
			p.cur.stsc = box.(*mp4.Stsc)
		}
	case mp4.BoxTypeStsz():
		if box, _, err := h.ReadPayload(); err == nil {
			p.cur.stsz = box.(*mp4.Stsz)
		}
	case mp4.BoxTypeStco():
		if box, _, err := h.ReadPayload(); err == nil {
			p.cur.stco = box.(*mp4.Stco)
		}
	case mp4.BoxTypeStss():
		if box, _, err := h.ReadPayload(); err == nil {
			p.cur.stss = box.(*mp4.Stss)
		}
	case mp4.BoxTypeColr():
		if box, _, err := h.ReadPayload(); err == nil {
			c := box.(*mp4.Colr)
			p.cur.track.Color = &ColorMeta{Primaries: c.ColourPrimaries,
				Transfer: c.TransferCharacteristics, Matrix: c.MatrixCoefficients,
				FullRange: c.FullRangeFlag}
		}
	default:
		if len(h.Path) >= 2 && h.Path[len(h.Path)-2] == mp4.BoxTypeStsd() {
			if err := p.sampleEntry(h); err != nil {
				return nil, err
			}
		}
	}
	return nil, nil
}

// sampleEntry handles one stsd child; unknown codecs fail explicitly.
func (p *parser) sampleEntry(h *mp4.ReadHandle) error {
	switch t := h.BoxInfo.Type.String(); t {
	case "avc1":
		if _, _, err := h.ReadPayload(); err != nil {
			return err
		}
		p.cur.track.Codec = "avc1"
		_, err := h.Expand()
		return err
	case "mp4a":
		box, _, err := h.ReadPayload()
		if err != nil {
			return err
		}
		a := box.(*mp4.AudioSampleEntry)
		p.cur.track.Codec = "mp4a"
		p.cur.track.Channels = int(a.ChannelCount)
		p.cur.track.SampleRate = int(a.SampleRate >> 16)
		return nil
	default:
		return model.New(model.StageProbe, "unknown_codec",
			fmt.Sprintf("unknown codec structure %q: failing explicitly", t),
			model.ExitDamagedInput)
	}
}

// build materializes the sample timeline from the raw stbl boxes.
func (b *trackBuilder) build() *model.MediaError {
	t := &b.track
	if b.stts == nil || b.stsz == nil || b.stsc == nil || b.stco == nil {
		return model.New(model.StageProbe, "incomplete_stbl",
			fmt.Sprintf("track %d: missing sample table boxes", t.ID), model.ExitDamagedInput)
	}
	n := int(b.stsz.SampleCount)
	sizes := b.stsz.EntrySize
	if b.stsz.SampleSize != 0 { // constant-size table
		sizes = make([]uint32, n)
		for i := range sizes {
			sizes[i] = b.stsz.SampleSize
		}
	}
	if len(sizes) < n {
		return model.New(model.StageProbe, "incomplete_stbl",
			fmt.Sprintf("track %d: stsz declares %d samples, has %d", t.ID, n, len(sizes)),
			model.ExitDamagedInput)
	}
	t.Samples = make([]Sample, n)
	var dts int64
	idx := 0
	for _, e := range b.stts.Entries {
		for k := uint32(0); k < e.SampleCount && idx < n; k++ {
			t.Samples[idx] = Sample{Index: idx, DTS: dts, PTS: dts,
				Duration: int64(e.SampleDelta), Size: int64(sizes[idx])}
			dts += int64(e.SampleDelta)
			idx++
		}
	}
	if b.ctts != nil { // composition offsets (version 1: signed)
		i := 0
		for _, e := range b.ctts.Entries {
			for k := uint32(0); k < e.SampleCount && i < n; k++ {
				t.Samples[i].PTS += int64(e.SampleOffsetV1)
				i++
			}
		}
	}
	// Chunk map: stsc runs over stco entries assign samples to offsets.
	chunk := 0
	sample := 0
	for ci, coff := range b.stco.ChunkOffset {
		for chunk+1 < len(b.stsc.Entries) && ci+1 >= int(b.stsc.Entries[chunk+1].FirstChunk) {
			chunk++
		}
		off := int64(coff)
		for k := uint32(0); k < b.stsc.Entries[chunk].SamplesPerChunk && sample < n; k++ {
			t.Samples[sample].Offset = off
			off += int64(sizes[sample])
			sample++
		}
	}
	if b.stss != nil {
		for _, s := range b.stss.SampleNumber {
			if int(s) <= n {
				t.Samples[s-1].Keyframe = true
			}
		}
	}
	return nil
}

// validate detects the first damaged sample: media data outside the
// mdat payload, or non-monotonic presentation timestamps.
func (p *parser) validate() *Damage {
	for _, b := range p.tracks {
		var prevPTS int64
		for i, s := range b.track.Samples {
			if p.mdatHi > 0 && (s.Offset < p.mdatLo || s.Offset+s.Size > p.mdatHi) {
				return &Damage{BoxType: "mdat", OffsetBytes: s.Offset,
					MediaTime: clock.New(s.DTS, b.track.Timescale)}
			}
			if i > 0 && s.PTS < prevPTS {
				return &Damage{BoxType: "ctts", OffsetBytes: s.Offset,
					MediaTime: clock.New(s.DTS, b.track.Timescale)}
			}
			prevPTS = s.PTS
		}
	}
	return nil
}
