package demux

import (
	"encoding/binary"
	"fmt"

	"fieldpost/internal/core"
)

// MP4Box is a located ISO-BMFF box header.
type MP4Box struct {
	Type       string
	Offset     int64
	HeaderSize int64
	Size       int64
}

var containerBoxes = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true,
	"stbl": true, "moof": true, "traf": true, "edts": true,
	"mvex": true, "udta": true,
}

// ParseMP4 parses CMAF/fMP4 boxes and recovers ordered trun samples. It rejects
// truncated data and illegal box lengths instead of approximating the timeline.
func ParseMP4(segmentID, source string, data []byte) (*core.Segment, error) {
	if len(data) < 8 {
		return nil, core.UnrecoverableError(core.StageDemux, source, segmentID, int64(len(data)),
			"truncated fMP4: need at least 8 bytes")
	}
	if err := validateBoxTree(segmentID, source, data, 0, int64(len(data)), 0); err != nil {
		return nil, err
	}
	tracks := parseMP4Tracks(data)
	if len(tracks) == 0 {
		tracks = []core.Track{
			{ID: 1, Kind: core.TrackVideo, Codec: "avc1", Timescale: 90000},
			{ID: 2, Kind: core.TrackAudio, Codec: "mp4a", Timescale: 48000},
		}
	}
	samples, err := parseFragments(segmentID, source, data, tracks)
	if err != nil {
		return nil, err
	}
	if len(samples) == 0 {
		return nil, core.UnrecoverableError(core.StageDemux, source, segmentID, 0, "fMP4 contains no trun samples")
	}
	if err := finalizeSamples(segmentID, source, tracks, samples); err != nil {
		return nil, err
	}
	return &core.Segment{ID: segmentID, Source: source, Tracks: tracks, Samples: samples}, nil
}

func readMP4Box(data []byte, off, limit int64) (MP4Box, error) {
	if off+8 > limit || off+8 > int64(len(data)) {
		return MP4Box{}, fmt.Errorf("truncated box header at %d", off)
	}
	size32 := binary.BigEndian.Uint32(data[off : off+4])
	b := MP4Box{Type: string(data[off+4 : off+8]), Offset: off, HeaderSize: 8}
	if size32 == 1 {
		if off+16 > limit {
			return b, fmt.Errorf("truncated largesize box at %d", off)
		}
		b.Size = int64(binary.BigEndian.Uint64(data[off+8 : off+16]))
		b.HeaderSize = 16
	} else if size32 == 0 {
		b.Size = limit - off
	} else {
		b.Size = int64(size32)
	}
	if b.Size < b.HeaderSize {
		return b, fmt.Errorf("illegal box size %d for %q at %d", b.Size, b.Type, off)
	}
	end := off + b.Size
	if end > limit || end > int64(len(data)) {
		return b, fmt.Errorf("truncated box %q: end %d beyond limit %d", b.Type, end, limit)
	}
	return b, nil
}

func validateBoxTree(segID, source string, data []byte, off, end int64, depth int) error {
	if depth > 32 {
		return core.UnrecoverableError(core.StageDemux, source, segID, off, "box nesting too deep")
	}
	for off < end {
		b, err := readMP4Box(data, off, end)
		if err != nil {
			return core.UnrecoverableError(core.StageDemux, source, segID, off, "%v", err)
		}
		if containerBoxes[b.Type] {
			if err := validateBoxTree(segID, source, data, off+b.HeaderSize, off+b.Size, depth+1); err != nil {
				return err
			}
		}
		off += b.Size
	}
	return nil
}

func findBoxes(data []byte, start, end int64, typ string, recursive bool) []MP4Box {
	var out []MP4Box
	off := start
	for off+8 <= end && off+8 <= int64(len(data)) {
		b, err := readMP4Box(data, off, end)
		if err != nil {
			return out
		}
		if b.Type == typ {
			out = append(out, b)
		}
		if recursive && containerBoxes[b.Type] {
			out = append(out, findBoxes(data, off+b.HeaderSize, off+b.Size, typ, true)...)
		}
		off += b.Size
	}
	return out
}

func parseMP4Tracks(data []byte) []core.Track {
	var tracks []core.Track
	for _, trak := range findBoxes(data, 0, int64(len(data)), "trak", true) {
		tr := core.Track{ID: 1, Timescale: 90000}
		if tkhd := firstBox(findBoxes(data, trak.Offset+trak.HeaderSize, trak.Offset+trak.Size, "tkhd", false)); tkhd != nil {
			tr.ID = binary.BigEndian.Uint32(data[tkhd.Offset+tkhd.HeaderSize+12:])
		}
		if mdhd := firstBox(findBoxes(data, trak.Offset+trak.HeaderSize, trak.Offset+trak.Size, "mdhd", true)); mdhd != nil {
			p := mdhd.Offset + mdhd.HeaderSize
			if data[p] == 1 {
				tr.Timescale = binary.BigEndian.Uint32(data[p+20:])
			} else {
				tr.Timescale = binary.BigEndian.Uint32(data[p+12:])
			}
		}
		if hdlr := firstBox(findBoxes(data, trak.Offset+trak.HeaderSize, trak.Offset+trak.Size, "hdlr", true)); hdlr != nil {
			h := string(data[hdlr.Offset+hdlr.HeaderSize+8 : hdlr.Offset+hdlr.HeaderSize+12])
			if h == "soun" {
				tr.Kind = core.TrackAudio
				tr.Codec = "mp4a"
			} else {
				tr.Kind = core.TrackVideo
				tr.Codec = "avc1"
			}
		}
		if stsd := firstBox(findBoxes(data, trak.Offset+trak.HeaderSize, trak.Offset+trak.Size, "stsd", true)); stsd != nil {
			entry := stsd.Offset + stsd.HeaderSize + 8 // fullbox + entry_count
			if entry+8 <= stsd.Offset+stsd.Size {
				codec := string(data[entry+4 : entry+8])
				if codec == "mp4a" {
					tr.Codec, tr.Kind = "mp4a", core.TrackAudio
				}
				if codec == "avc1" || codec == "avc3" {
					tr.Codec, tr.Kind = codec, core.TrackVideo
				}
			}
		}
		tracks = append(tracks, tr)
	}
	return tracks
}

func firstBox(bs []MP4Box) *MP4Box {
	if len(bs) == 0 {
		return nil
	}
	return &bs[0]
}

func parseFragments(segID, source string, data []byte, tracks []core.Track) ([]core.Sample, error) {
	var out []core.Sample
	moofs := findBoxes(data, 0, int64(len(data)), "moof", false)
	for _, moof := range moofs {
		trafs := findBoxes(data, moof.Offset+moof.HeaderSize, moof.Offset+moof.Size, "traf", false)
		for _, traf := range trafs {
			tfhd := firstBox(findBoxes(data, traf.Offset+traf.HeaderSize, traf.Offset+traf.Size, "tfhd", false))
			tfdt := firstBox(findBoxes(data, traf.Offset+traf.HeaderSize, traf.Offset+traf.Size, "tfdt", false))
			trun := firstBox(findBoxes(data, traf.Offset+traf.HeaderSize, traf.Offset+traf.Size, "trun", false))
			if tfhd == nil || tfdt == nil || trun == nil {
				return nil, core.UnrecoverableError(core.StageDemux, source, segID, traf.Offset,
					"traf missing tfhd/tfdt/trun")
			}
			trackID := binary.BigEndian.Uint32(data[tfhd.Offset+tfhd.HeaderSize+4:])
			p := tfdt.Offset + tfdt.HeaderSize
			var baseDTS int64
			if data[p] == 1 {
				baseDTS = int64(binary.BigEndian.Uint64(data[p+4:]))
			} else {
				baseDTS = int64(binary.BigEndian.Uint32(data[p+4:]))
			}
			tp := trun.Offset + trun.HeaderSize
			flags := uint32(data[tp+1])<<16 | uint32(data[tp+2])<<8 | uint32(data[tp+3])
			count := binary.BigEndian.Uint32(data[tp+4:])
			cursor := tp + 8
			var dataPos int64 = -1
			if flags&0x000001 != 0 {
				dataPos = int64(int32(binary.BigEndian.Uint32(data[cursor:])))
				cursor += 4
			}
			if flags&0x000004 != 0 { // first-sample-flags-present
				cursor += 4
			}
			for i := uint32(0); i < count; i++ {
				if cursor+12 > trun.Offset+trun.Size {
					return nil, core.UnrecoverableError(core.StageDemux, source, segID, trun.Offset,
						"trun entry %d truncated", i)
				}
				dur := core.Ticks(binary.BigEndian.Uint32(data[cursor:]))
				size := int64(binary.BigEndian.Uint32(data[cursor+4:]))
				sflags := decodeMP4SampleFlags(binary.BigEndian.Uint32(data[cursor+8:]))
				cursor += 12
				if dataPos < 0 {
					return nil, core.UnrecoverableError(core.StageDemux, source, segID, trun.Offset,
						"trun lacks data offset")
				}
				if dataPos+size > int64(len(data)) || size < 0 {
					return nil, core.UnrecoverableError(core.StageDemux, source, segID, dataPos,
						"sample %d data range out of bounds size=%d", i, size)
				}
				out = append(out, core.Sample{
					TrackID: trackID, PTS: core.Ticks(baseDTS), DTS: core.Ticks(baseDTS),
					Duration: dur, Flags: sflags,
					Data: append([]byte(nil), data[dataPos:dataPos+size]...), DataOffset: dataPos,
				})
				baseDTS += int64(dur)
				dataPos += size
			}
		}
	}
	return out, nil
}

func encodeMP4SampleFlags(f uint32) uint32 {
	out := f & 0x0000ffff // preserve canonical bits in degradation priority
	if f&core.FlagSync == 0 {
		out |= 0x00010000 // sample_is_non_sync_sample
	}
	return out
}

func decodeMP4SampleFlags(f uint32) uint32 {
	canonical := f & 0x0000ffff
	if f&0x00010000 == 0 {
		canonical |= core.FlagSync
	} else {
		canonical |= core.FlagNonSync
	}
	return canonical
}
