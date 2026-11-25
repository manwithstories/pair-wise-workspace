package container

import (
	"encoding/binary"
	"fmt"
	"strings"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/model"
)

// claimTarget maps a metadata tag to the claim layer it writes.
func claimTarget(tag uint16) (func(*model.Track, string), bool) {
	set := func(src model.Source, key string) func(*model.Track, string) {
		return func(t *model.Track, v string) { setClaim(t, src, key, v) }
	}
	switch tag {
	case tagContainerRot:
		return set(model.SourceContainer, "rotation"), true
	case tagBitRot:
		return set(model.SourceBitstream, "rotation"), true
	case tagContainerCrop:
		return set(model.SourceContainer, "crop"), true
	case tagBitCrop:
		return set(model.SourceBitstream, "crop"), true
	case tagContainerPrim:
		return set(model.SourceContainer, "primaries"), true
	case tagContainerXfer:
		return set(model.SourceContainer, "transfer"), true
	case tagContainerMat:
		return set(model.SourceContainer, "matrix"), true
	case tagContainerRange:
		return set(model.SourceContainer, "range"), true
	case tagBitPrim:
		return set(model.SourceBitstream, "primaries"), true
	case tagBitXfer:
		return set(model.SourceBitstream, "transfer"), true
	case tagBitMat:
		return set(model.SourceBitstream, "matrix"), true
	case tagBitRange:
		return set(model.SourceBitstream, "range"), true
	case tagContainerMarker:
		return set(model.SourceContainer, "color"), true
	case tagBitMarker:
		return set(model.SourceBitstream, "color"), true
	case tagContainerW:
		return set(model.SourceContainer, "width"), true
	case tagContainerH:
		return set(model.SourceContainer, "height"), true
	case tagBitW:
		return set(model.SourceBitstream, "width"), true
	case tagBitH:
		return set(model.SourceBitstream, "height"), true
	case tagBitAperture:
		return set(model.SourceBitstream, "aperture"), true
	}
	return nil, false
}

func decodeClaim(tag uint16, v []byte, off int64) (string, *fault.Error) {
	switch tag {
	case tagContainerRot, tagBitRot:
		if len(v) != 2 {
			return "", fault.New(-1, off, fault.KindCorrupt, true, "rotation field has %d bytes", len(v))
		}
		return fmt.Sprint(int16(binary.BigEndian.Uint16(v))), nil
	case tagContainerCrop, tagBitCrop:
		if len(v) != 8 {
			return "", fault.New(-1, off, fault.KindCorrupt, true, "crop field has %d bytes", len(v))
		}
		return fmt.Sprintf("%d,%d,%d,%d",
			binary.BigEndian.Uint16(v[0:2]), binary.BigEndian.Uint16(v[2:4]),
			binary.BigEndian.Uint16(v[4:6]), binary.BigEndian.Uint16(v[6:8])), nil
	case tagContainerPrim, tagContainerXfer, tagContainerMat,
		tagBitPrim, tagBitXfer, tagBitMat:
		if len(v) != 2 {
			return "", fault.New(-1, off, fault.KindCorrupt, true, "colour coefficient has %d bytes", len(v))
		}
		return fmt.Sprint(binary.BigEndian.Uint16(v)), nil
	case tagContainerW, tagContainerH, tagBitW, tagBitH:
		if len(v) != 4 {
			return "", fault.New(-1, off, fault.KindCorrupt, true, "dimension field has %d bytes", len(v))
		}
		return fmt.Sprint(binary.BigEndian.Uint32(v)), nil
	case tagBitAperture:
		if len(v) != 8 {
			return "", fault.New(-1, off, fault.KindCorrupt, true, "aperture field has %d bytes", len(v))
		}
		return fmt.Sprintf("%dx%d", binary.BigEndian.Uint32(v[0:4]), binary.BigEndian.Uint32(v[4:8])), nil
	case tagContainerRange, tagBitRange, tagContainerMarker, tagBitMarker:
		return strings.TrimSpace(string(v)), nil
	}
	return "", nil
}

func (p *mxfParser) parseIndexTable(pos int64, value []byte) {
	var trackID uint32
	var entries []byte
	var count int64
	p.localSet(pos, value, func(tag uint16, v []byte, off int64) *fault.Error {
		switch tag {
		case tagTrackID:
			if len(v) == 4 {
				trackID = binary.BigEndian.Uint32(v)
			}
		case tagIndexCount:
			if len(v) == 4 {
				count = int64(binary.BigEndian.Uint32(v))
			}
		case tagIndexEntries:
			entries = v
		}
		return nil
	})
	if trackID == 0 {
		p.faults.Add(fault.New(-1, pos, fault.KindCorrupt, true,
			"index table segment at offset %d has no track ID", pos))
		return
	}
	if count > int64(p.lim.MaxSamples) {
		p.faults.Add(fault.New(-1, pos, fault.KindLimit, false,
			"index table declares %d entries, exceeding limit %d", count, p.lim.MaxSamples))
		return
	}
	if int64(len(entries)) != count*mxfEntrySize {
		p.faults.Add(fault.New(-1, pos, fault.KindCorrupt, false,
			"index table for track %d declares %d entries but carries %d bytes",
			trackID, count, len(entries)))
		return
	}
	out := make([]IndexEntry, 0, count)
	for i := int64(0); i < count; i++ {
		b := entries[i*mxfEntrySize : (i+1)*mxfEntrySize]
		out = append(out, IndexEntry{
			StreamOffset:  binary.BigEndian.Uint32(b[0:4]),
			Duration:      binary.BigEndian.Uint32(b[4:8]),
			EditUnitDelta: binary.BigEndian.Uint32(b[8:12]),
			Key:           binary.BigEndian.Uint32(b[12:16])&1 == 1,
		})
	}
	if p.indexSeen[trackID] {
		p.faults.Add(fault.New(-1, pos, fault.KindCorrupt, true,
			"duplicate index table segment for track %d", trackID))
		return
	}
	p.indexSeen[trackID] = true
	p.indexEntries[trackID] = out
}

func (p *mxfParser) parseEssence(pos, valueStart int64, value []byte) {
	if len(value) < 4 {
		p.faults.Add(fault.New(-1, pos, fault.KindTruncated, true,
			"picture essence element at offset %d has no track number", pos))
		return
	}
	trackID := binary.BigEndian.Uint32(value[0:4])
	p.essence = append(p.essence, mxfEssence{
		trackID: trackID,
		off:     valueStart + 4,
		size:    int64(len(value) - 4),
	})
}

// buildTracks assembles sample tables from descriptors, essence and index
// tables, validating index ownership and offsets.
func (p *mxfParser) buildTracks() {
	for _, id := range p.order {
		st := p.tracks[id]
		trak := st.track
		var items []mxfEssence
		for _, e := range p.essence {
			if e.trackID == id {
				items = append(items, e)
			}
		}
		if len(items) > p.lim.MaxSamples {
			p.faults.Add(fault.New(trak.Number, 0, fault.KindLimit, false,
				"track %d has %d essence items, exceeding limit %d", id, len(items), p.lim.MaxSamples))
			items = items[:p.lim.MaxSamples]
		}
		trak.Samples = make([]model.Sample, len(items))
		var dts int64
		for i, e := range items {
			trak.Samples[i] = model.Sample{
				Index: i, TrackID: id, Offset: e.off, Size: e.size,
				DTS: dts, Duration: 1, Sync: true, Codec: trak.Codec,
			}
			dts++
		}
		entries := p.indexEntries[id]
		if len(entries) > 0 {
			p.applyIndex(trak, entries)
		} else if trak.Kind == model.Video {
			p.faults.Add(fault.New(trak.Number, 0, fault.KindGap, true,
				"track %d has no index table; timing synthesised from edit rate", id))
		}
		if trak.Duration == 0 && len(trak.Samples) > 0 {
			last := trak.Samples[len(trak.Samples)-1]
			trak.Duration = last.DTS + last.Duration
		}
		p.file.Tracks = append(p.file.Tracks, trak)
	}
	// Essence whose track is unknown is kept as a diagnostic fault.
	known := map[uint32]bool{}
	for id := range p.tracks {
		known[id] = true
	}
	for _, e := range p.essence {
		if !known[e.trackID] {
			p.faults.Add(fault.New(-1, e.off, fault.KindOutOfRange, true,
				"essence at offset %d references unknown track %d", e.off, e.trackID))
		}
	}
}

// applyIndex attaches index-table timing to samples, rejecting entries whose
// stream offset does not point at an essence item of the same track.
func (p *mxfParser) applyIndex(trak *model.Track, entries []IndexEntry) {
	byOffset := map[uint32]int{}
	for _, s := range trak.Samples {
		if s.Offset <= int64(^uint32(0)) {
			byOffset[uint32(s.Offset)] = s.Index
		}
	}
	var dts int64
	for i, e := range entries {
		idx, ok := byOffset[e.StreamOffset]
		if !ok {
			p.faults.Add(fault.New(trak.Number, int64(e.StreamOffset), fault.KindOutOfRange, true,
				"index entry %d references stream offset %d that belongs to no %s sample",
				i, e.StreamOffset, trak.Label()))
			continue
		}
		dur := int64(e.Duration)
		if dur <= 0 {
			dur = 1
		}
		s := &trak.Samples[idx]
		s.DTS = dts
		s.Duration = dur
		s.Sync = e.Key
		dts += dur
	}
}

// mxfFileBytes is a simple length sanity helper used by writers and tests.
func mxfFileBytes(parts ...[]byte) int64 {
	var n int64
	for _, p := range parts {
		n += int64(len(p))
	}
	return n
}
