package ingest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	gomp4 "github.com/abema/go-mp4"

	"fieldcut/internal/clock"
)

// ReadFragmentedMP4 parses the deterministic moof/mdat subset with go-mp4 box
// headers. Declared boxes that run past data and short mdat payloads produce a
// structured truncated_mp4 error carrying the track and byte offset.
func ReadFragmentedMP4(data []byte, expectedTrackID int) (*MP4Track, error) {
	if len(data) > MaxMP4Bytes {
		return nil, clock.Errorf(clock.CodeConfig, expectedTrackID, "MP4 exceeds read cap: %d", len(data))
	}
	track := &MP4Track{TrackID: expectedTrackID}
	r := bytes.NewReader(data)
	fileSize := uint64(len(data))

	for {
		pos, _ := r.Seek(0, io.SeekCurrent)
		if pos == int64(len(data)) {
			break
		}
		bi, err := gomp4.ReadBoxInfo(r)
		if err != nil {
			return nil, truncatedError(expectedTrackID, pos, "short box header: "+err.Error())
		}
		if bi.Size < bi.HeaderSize || !rangeWithin(fileSize, bi.Offset, bi.Size) {
			return nil, truncatedError(expectedTrackID, int64(bi.Offset),
				fmt.Sprintf("box %s declared size %d ends beyond file size %d", bi.Type.String(), bi.Size, fileSize))
		}
		boxStart := bi.Offset
		payloadStart := bi.Offset + bi.HeaderSize
		payloadEnd := bi.Offset + bi.Size

		switch bi.Type.String() {
		case "ftsc":
			if payloadEnd-payloadStart >= 4 {
				track.TimeScale = binary.BigEndian.Uint32(data[payloadStart : payloadStart+4])
			}
		case "colr":
			track.Color = parseColor(data[payloadStart:payloadEnd])
		case "moof":
			fragTrack, base, entries, dataOffset, err := parseMoof(data, bi)
			if err != nil {
				return nil, err
			}
			if fragTrack != 0 {
				track.TrackID = int(fragTrack)
			}
			if track.TimeScale == 0 {
				return nil, clock.Errorf(clock.CodeInvalidArgument, track.TrackID, "missing ftsc timescale before moof")
			}
			dataPos := int64(boxStart) + int64(dataOffset)
			if dataPos < 0 || dataPos > int64(len(data)) {
				return nil, truncatedError(track.TrackID, int64(boxStart), "moof data offset points outside file")
			}
			ticks := base
			const maxInt64Ticks = uint64(1<<63 - 1)
			for i, en := range entries {
				if ticks > maxInt64Ticks {
					return nil, &clock.Error{Code: clock.CodeOverflow, TrackID: track.TrackID,
						OriginalTimestamp: int64(en.duration), OriginalValue: "tfdt/trun decode time overflow",
						Message: "sample timestamp exceeds signed nanosecond range"}
				}
				end := dataPos + int64(en.size)
				if en.size == 0 || end > int64(len(data)) {
					return track, truncatedError(track.TrackID, dataPos,
						fmt.Sprintf("mdat sample %d size %d is truncated", i, en.size))
				}
				pts, err := clock.TicksToNs(int64(ticks), 1, int64(track.TimeScale))
				if err != nil {
					return nil, &clock.Error{Code: clock.CodeOverflow, TrackID: track.TrackID,
						OriginalTimestamp: int64(en.duration), Message: "sample PTS overflow"}
				}
				track.Samples = append(track.Samples, MP4Sample{
					PTS: pts, Duration: durationToNs(en.duration, track.TimeScale),
					Data: append([]byte(nil), data[dataPos:end]...), Sync: i == 0,
				})
				if len(track.Samples) > MaxMP4Samples {
					return nil, clock.Errorf(clock.CodeConfig, track.TrackID, "too many MP4 samples")
				}
				dataPos = end
				if uint64(en.duration) > maxInt64Ticks-ticks {
					return nil, &clock.Error{Code: clock.CodeOverflow, TrackID: track.TrackID,
						OriginalTimestamp: int64(en.duration), Message: "cumulative MP4 timestamp overflows"}
				}
				ticks += uint64(en.duration)
			}
		}

		if _, err := bi.SeekToEnd(r); err != nil {
			return nil, err
		}
	}
	if len(track.Samples) == 0 {
		return nil, clock.Errorf(clock.CodeInvalidArgument, track.TrackID, "MP4 contains no samples")
	}
	return track, nil
}

func durationToNs(ticks uint32, timescale uint32) clock.Ns {
	ns, _ := clock.TicksToNs(int64(ticks), 1, int64(timescale))
	return ns
}

func rangeWithin(length, offset, size uint64) bool {
	return offset <= length && size <= length-offset
}

func truncatedError(trackID int, offset int64, message string) *clock.Error {
	return &clock.Error{
		Code: clock.CodeTruncatedMP4, TrackID: trackID,
		OriginalValue: fmt.Sprintf("byte_offset=%d", offset),
		Message:       message,
	}
}

type trunEntry struct {
	duration uint32
	size     uint32
}

func parseMoof(data []byte, bi *gomp4.BoxInfo) (trackID uint32, base uint64, entries []trunEntry, dataOffset int32, err error) {
	payload := data[bi.Offset+bi.HeaderSize : bi.Offset+bi.Size]
	r := bytes.NewReader(payload)
	for {
		pos, _ := r.Seek(0, io.SeekCurrent)
		if pos == int64(len(payload)) {
			break
		}
		child, e := gomp4.ReadBoxInfo(r)
		if e != nil {
			return 0, 0, nil, 0, truncatedError(-1, int64(bi.Offset)+pos, "short moof child header")
		}
		cs, ce := child.Offset+child.HeaderSize, child.Offset+child.Size
		if child.Size < child.HeaderSize || !rangeWithin(uint64(len(payload)), child.Offset, child.Size) {
			return 0, 0, nil, 0, truncatedError(-1, int64(bi.Offset)+int64(child.Offset),
				fmt.Sprintf("moof child %s is truncated", child.Type.String()))
		}
		childPayload := payload[cs:ce]
		switch child.Type.String() {
		case "traf":
			trackID, base, entries, dataOffset, err = parseTraf(childPayload)
			if err != nil {
				return 0, 0, nil, 0, err
			}
		}
		if _, e := child.SeekToEnd(r); e != nil {
			return 0, 0, nil, 0, e
		}
	}
	if len(entries) == 0 {
		return 0, 0, nil, 0, clock.Errorf(clock.CodeInvalidArgument, -1, "moof contains no trun entries")
	}
	return trackID, base, entries, dataOffset, nil
}

func parseTraf(payload []byte) (trackID uint32, base uint64, entries []trunEntry, dataOffset int32, err error) {
	r := bytes.NewReader(payload)
	for {
		pos, _ := r.Seek(0, io.SeekCurrent)
		if pos == int64(len(payload)) {
			break
		}
		box, e := gomp4.ReadBoxInfo(r)
		if e != nil {
			return 0, 0, nil, 0, clock.Errorf(clock.CodeTruncatedMP4, -1, "short traf child header")
		}
		s, end := box.Offset+box.HeaderSize, box.Offset+box.Size
		if box.Size < box.HeaderSize || !rangeWithin(uint64(len(payload)), box.Offset, box.Size) {
			return 0, 0, nil, 0, clock.Errorf(clock.CodeTruncatedMP4, -1, "traf child %s truncated", box.Type.String())
		}
		p := payload[s:end]
		switch box.Type.String() {
		case "tfhd":
			if len(p) >= 8 { // FullBox 4 + trackID 4
				trackID = binary.BigEndian.Uint32(p[4:8])
			}
		case "tfdt":
			base, e = parseTFDT(p)
			if e != nil {
				return 0, 0, nil, 0, e
			}
		case "trun":
			entries, dataOffset, e = parseTRUN(p)
			if e != nil {
				return 0, 0, nil, 0, e
			}
		}
		if _, e := box.SeekToEnd(r); e != nil {
			return 0, 0, nil, 0, e
		}
	}
	return trackID, base, entries, dataOffset, nil
}

func parseTFDT(p []byte) (uint64, error) {
	if len(p) < 8 {
		return 0, clock.Errorf(clock.CodeTruncatedMP4, -1, "short tfdt")
	}
	version := p[0]
	if version == 1 {
		if len(p) < 12 {
			return 0, clock.Errorf(clock.CodeTruncatedMP4, -1, "short v1 tfdt")
		}
		return binary.BigEndian.Uint64(p[4:12]), nil
	}
	return uint64(binary.BigEndian.Uint32(p[4:8])), nil
}

func parseTRUN(p []byte) ([]trunEntry, int32, error) {
	if len(p) < 8 {
		return nil, 0, clock.Errorf(clock.CodeTruncatedMP4, -1, "short trun")
	}
	flags := uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
	count := binary.BigEndian.Uint32(p[4:8])
	if count > MaxMP4Samples {
		return nil, 0, clock.Errorf(clock.CodeConfig, -1, "trun sample count %d exceeds cap %d", count, MaxMP4Samples)
	}
	off := 8
	var dataOffset int32
	if flags&0x000001 != 0 {
		if len(p) < off+4 {
			return nil, 0, clock.Errorf(clock.CodeTruncatedMP4, -1, "short trun data offset")
		}
		dataOffset = int32(binary.BigEndian.Uint32(p[off : off+4]))
		off += 4
	}
	if flags&0x000004 != 0 {
		off += 4
	}
	entries := make([]trunEntry, 0, count)
	for i := uint32(0); i < count; i++ {
		var en trunEntry
		if flags&0x000100 != 0 {
			if len(p) < off+4 {
				return nil, 0, clock.Errorf(clock.CodeTruncatedMP4, -1, "short trun duration")
			}
			en.duration = binary.BigEndian.Uint32(p[off : off+4])
			off += 4
		}
		if flags&0x000200 != 0 {
			if len(p) < off+4 {
				return nil, 0, clock.Errorf(clock.CodeTruncatedMP4, -1, "short trun size")
			}
			en.size = binary.BigEndian.Uint32(p[off : off+4])
			off += 4
		}
		if flags&0x000400 != 0 {
			off += 4
		}
		if flags&0x000800 != 0 {
			off += 4
		}
		entries = append(entries, en)
	}
	return entries, dataOffset, nil
}

func parseColor(p []byte) ColorInfo {
	c := ColorInfo{HasMetadata: len(p) >= 4}
	if len(p) < 4 || string(p[0:4]) != "nclx" || len(p) < 11 {
		return c
	}
	c.Primaries = colorName(binary.BigEndian.Uint16(p[4:6]), map[uint16]string{1: "bt709", 9: "bt2020"})
	c.Transfer = colorName(binary.BigEndian.Uint16(p[6:8]), map[uint16]string{1: "bt709", 16: "smpte2084"})
	c.Matrix = colorName(binary.BigEndian.Uint16(p[8:10]), map[uint16]string{1: "bt709", 9: "bt2020nc"})
	c.FullRange = p[10]&0x80 != 0
	return c
}

func colorName(code uint16, names map[uint16]string) string {
	if n, ok := names[code]; ok {
		return n
	}
	return "unknown"
}
