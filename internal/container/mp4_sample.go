package container

import (
	"encoding/binary"
	"fmt"

	"github.com/abema/go-mp4"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/model"
)

// visualEntryFixed is the size of the fixed fields of a VisualSampleEntry
// after its own 8-byte box header.
const visualEntryFixed = 70

// parseStsd walks sample description entries and decodes the first one.
func (p *mp4Parser) parseStsd(b box, start, end int64, st *trakState) {
	if end-start < 8 {
		p.faults.Add(fault.New(st.track.Number, b.offset, fault.KindTruncated, false, "stsd header truncated"))
		return
	}
	count := binary.BigEndian.Uint32(p.data[start+4 : start+8])
	if count == 0 {
		p.faults.Add(fault.New(st.track.Number, b.offset, fault.KindCorrupt, false, "stsd has no entries"))
		return
	}
	pos := start + 8
	for i := uint32(0); i < count; i++ {
		entry, ferr := p.readBox(pos, end)
		if ferr != nil {
			p.faults.Add(ferr.WithTrack(st.track.Number))
			return
		}
		if i == 0 {
			p.parseSampleEntry(entry, pos+entry.header, end, st)
		}
		pos += entry.size
	}
}

func (p *mp4Parser) parseSampleEntry(b box, start, end int64, st *trakState) {
	st.track.Codec = codecFor(b.typ)
	if st.kind != model.Video {
		return
	}
	if end-start < visualEntryFixed {
		p.faults.Add(fault.New(st.track.Number, b.offset, fault.KindTruncated, false,
			"visual sample entry %q truncated", b.typ))
		return
	}
	w := int(binary.BigEndian.Uint16(p.data[start+24 : start+26]))
	h := int(binary.BigEndian.Uint16(p.data[start+26 : start+28]))
	switch {
	case w == 0 || h == 0:
		p.faults.Add(fault.New(st.track.Number, b.offset, fault.KindCorrupt, true,
			"visual sample entry %q has zero coded dimensions", b.typ))
	case w > p.lim.MaxImageDim || h > p.lim.MaxImageDim:
		p.faults.Add(fault.New(st.track.Number, b.offset, fault.KindLimit, false,
			"coded dimension %dx%d exceeds limit %d", w, h, p.lim.MaxImageDim))
	default:
		setClaim(st.track, model.SourceBitstream, "width", fmt.Sprint(w))
		setClaim(st.track, model.SourceBitstream, "height", fmt.Sprint(h))
	}
	child := start + visualEntryFixed
	for child+8 <= end {
		cb, ferr := p.readBox(child, end)
		if ferr != nil {
			return
		}
		switch cb.typ {
		case "clap":
			p.parseClap(cb, child+cb.header, end, st.track)
		case "colr":
			p.parseColr(cb, child+cb.header, end, st.track, model.SourceBitstream)
		}
		child += cb.size
	}
}

func codecFor(fourCC string) model.Codec {
	switch fourCC {
	case "mjpg", "mjpa", "jpeg", "MJPEG":
		return model.CodecMJPEG
	case "avc1", "avc3":
		return model.CodecH264
	case "mp4a":
		return model.CodecAAC
	case "sowt", "twos", "lpcm":
		return model.CodecPCM
	}
	return model.Codec(fourCC)
}

// parseClap reads the clean aperture: four 16.16 fixed-point numerator/
// denominator pairs (widthN, widthD, heightN, heightD).
func (p *mp4Parser) parseClap(b box, start, end int64, trak *model.Track) {
	if end-start < 32 {
		p.faults.Add(fault.New(trak.Number, b.offset, fault.KindTruncated, true, "clap box truncated"))
		return
	}
	get := func(i int64) (int64, bool) {
		n := int64(int32(binary.BigEndian.Uint32(p.data[start+i*8 : start+i*8+4])))
		d := int64(int32(binary.BigEndian.Uint32(p.data[start+i*8+4 : start+i*8+8])))
		if d <= 0 {
			return 0, false
		}
		if n%d != 0 {
			return 0, false
		}
		return n / d, true
	}
	w, okW := get(0)
	h, okH := get(2)
	if !okW || !okH || w < 0 || h < 0 {
		p.faults.Add(fault.New(trak.Number, b.offset, fault.KindCorrupt, true,
			"clap aperture is malformed"))
		return
	}
	setClaim(trak, model.SourceBitstream, "aperture", fmt.Sprintf("%dx%d", w, h))
}

func (p *mp4Parser) parseStts(b box, start, end int64, st *trakState) {
	var stts mp4.Stts
	if err := p.unmarshal(p.data[start:end], &stts); err != nil {
		p.faults.Add(fault.Wrap(st.track.Number, b.offset, fault.KindCorrupt, false, err))
		return
	}
	st.hadStts = true
	total := 0
	for _, e := range stts.Entries {
		total += int(e.SampleCount)
		if total > p.lim.MaxSamples {
			p.faults.Add(fault.New(st.track.Number, b.offset, fault.KindLimit, false,
				"stts sample count exceeds limit %d", p.lim.MaxSamples))
			return
		}
	}
	st.track.Samples = make([]model.Sample, 0, total)
	var dts int64
	for _, e := range stts.Entries {
		for i := uint32(0); i < e.SampleCount; i++ {
			st.track.Samples = append(st.track.Samples, model.Sample{
				Index: len(st.track.Samples), DTS: dts, Duration: int64(e.SampleDelta), Sync: true,
			})
			dts += int64(e.SampleDelta)
		}
	}
}

func (p *mp4Parser) parseStsz(b box, start, end int64, st *trakState) {
	var stsz mp4.Stsz
	if err := p.unmarshal(p.data[start:end], &stsz); err != nil {
		p.faults.Add(fault.Wrap(st.track.Number, b.offset, fault.KindCorrupt, false, err))
		return
	}
	st.hadStsz = true
	n := int(stsz.SampleCount)
	if n > p.lim.MaxSamples {
		p.faults.Add(fault.New(st.track.Number, b.offset, fault.KindLimit, false,
			"sample count %d exceeds limit %d", n, p.lim.MaxSamples))
		return
	}
	if len(st.track.Samples) != n {
		p.faults.Add(fault.New(st.track.Number, b.offset, fault.KindCorrupt, true,
			"stsz declares %d samples but stts describes %d", n, len(st.track.Samples)))
	}
	for i := 0; i < n; i++ {
		size := int64(stsz.SampleSize)
		if stsz.SampleSize == 0 {
			if i >= len(stsz.EntrySize) {
				p.faults.Add(fault.New(st.track.Number, b.offset, fault.KindCorrupt, false,
					"stsz is missing entry %d of %d", i, n))
				return
			}
			size = int64(stsz.EntrySize[i])
		}
		if i < len(st.track.Samples) {
			st.track.Samples[i].Size = size
		}
	}
}

func (p *mp4Parser) parseStco(b box, start, end int64, st *trakState) {
	var stco mp4.Stco
	if err := p.unmarshal(p.data[start:end], &stco); err != nil {
		p.faults.Add(fault.Wrap(st.track.Number, b.offset, fault.KindCorrupt, false, err))
		return
	}
	st.hadStco = true
	st.chunkOffsets = append(st.chunkOffsets, stco.ChunkOffset...)
}

func (p *mp4Parser) parseStsc(b box, start, end int64, st *trakState) {
	var stsc mp4.Stsc
	if err := p.unmarshal(p.data[start:end], &stsc); err != nil {
		p.faults.Add(fault.Wrap(st.track.Number, b.offset, fault.KindCorrupt, false, err))
		return
	}
	st.stsc = append(st.stsc, stsc.Entries...)
}

func (p *mp4Parser) parseStss(b box, start, end int64, st *trakState) {
	var stss mp4.Stss
	if err := p.unmarshal(p.data[start:end], &stss); err != nil {
		p.faults.Add(fault.Wrap(st.track.Number, b.offset, fault.KindCorrupt, true, err))
		st.syncSet = map[int]bool{}
		return
	}
	if len(stss.SampleNumber) > p.lim.MaxSync {
		p.faults.Add(fault.New(st.track.Number, b.offset, fault.KindLimit, true,
			"sync table has %d entries, exceeding limit %d", len(stss.SampleNumber), p.lim.MaxSync))
		stss.SampleNumber = stss.SampleNumber[:p.lim.MaxSync]
	}
	// A present stss replaces the implicit "every sample is a sync point".
	st.syncSet = map[int]bool{}
	for _, s := range stss.SampleNumber {
		idx := int(s) - 1
		if idx < 0 || idx >= len(st.track.Samples) {
			p.faults.Add(fault.New(st.track.Number, b.offset, fault.KindOutOfRange, true,
				"stss sample number %d outside sample count %d", s, len(st.track.Samples)))
			continue
		}
		st.syncSet[idx] = true
	}
}
