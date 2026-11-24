package boxes

import (
	"encoding/binary"
	"os"

	"github.com/abema/go-mp4"

	"bcast/internal/media"
)

const (
	trunDataOffsetPresent    = 0x000001
	trunFirstSampleFlags     = 0x000004
	trunSampleDuration       = 0x000100
	trunSampleSize           = 0x000200
	trunSampleFlags          = 0x000400
	trunSampleCompositionOff = 0x000800
	nonSyncSampleFlag        = uint32(0x00010000)
)

func parseMoof(f *os.File, bi *mp4.BoxInfo, frag *Fragment, timescales map[uint32]uint32) error {
	frag.MoofOffset = int64(bi.Offset)
	frag.MoofSize = int64(bi.Size)
	payloadStart := frag.MoofOffset + int64(bi.HeaderSize)
	end := frag.MoofOffset + frag.MoofSize

	return forChildren(f, payloadStart, end, func(child *mp4.BoxInfo) error {
		switch boxType(child.Type) {
		case "mfhd":
			buf, _, err := readPayload(f, child, 1024)
			if err != nil {
				return err
			}
			if len(buf) < 8 {
				return &boxScanError{offset: int64(child.Offset), kind: "short_mfhd"}
			}
			frag.Sequence = beUint32(buf[4:8])
		case "traf":
			if err := parseTraf(f, child, frag); err != nil {
				return err
			}
		}
		return nil
	})
}

func parseTraf(f *os.File, traf *mp4.BoxInfo, frag *Fragment) error {
	start := int64(traf.Offset) + int64(traf.HeaderSize)
	end := int64(traf.Offset) + int64(traf.Size)
	return forChildren(f, start, end, func(child *mp4.BoxInfo) error {
		switch boxType(child.Type) {
		case "tfhd":
			buf, _, err := readPayload(f, child, 1024)
			if err != nil {
				return err
			}
			if len(buf) < 8 {
				return &boxScanError{offset: int64(child.Offset), kind: "short_tfhd"}
			}
			frag.TrackID = beUint32(buf[4:8])
		case "tfdt":
			buf, boxStart, err := readPayload(f, child, 1024)
			if err != nil {
				return err
			}
			if len(buf) < 8 {
				return &boxScanError{offset: int64(child.Offset), kind: "short_tfdt"}
			}
			version := buf[0]
			if version == 1 {
				if len(buf) < 12 {
					return &boxScanError{offset: int64(child.Offset), kind: "short_tfdt_v1"}
				}
				frag.TFDT = beUint64(buf[4:12])
				frag.TFDTValueOff = boxStart + int64(child.HeaderSize) + 4
				frag.TFDTValueWidth = 8
			} else {
				frag.TFDT = uint64(beUint32(buf[4:8]))
				frag.TFDTValueOff = boxStart + int64(child.HeaderSize) + 4
				frag.TFDTValueWidth = 4
			}
		case "trun":
			if err := parseTrun(f, child, frag); err != nil {
				return err
			}
		}
		return nil
	})
}

func parseTrun(f *os.File, bi *mp4.BoxInfo, frag *Fragment) error {
	buf, boxStart, err := readPayload(f, bi, MaxMetadataBox)
	if err != nil {
		return err
	}
	if len(buf) < 8 {
		return &boxScanError{offset: boxStart, kind: "short_trun"}
	}
	version := buf[0]
	flags := beUint24(buf[1:4])
	count := beUint32(buf[4:8])
	if count > 1_000_000 || int64(count)*96 > media.MaxBatchMemory {
		return &boxScanError{offset: boxStart + 4, kind: "trun_sample_count_exceeds_memory_budget"}
	}
	pos := 8
	var dataOffset int32
	need := func(n int) error {
		if pos+n > len(buf) {
			return &boxScanError{offset: boxStart + int64(bi.HeaderSize) + int64(pos), kind: "truncated_trun_entry"}
		}
		return nil
	}
	if flags&trunDataOffsetPresent != 0 {
		if err := need(4); err != nil {
			return err
		}
		dataOffset = int32(binary.BigEndian.Uint32(buf[pos : pos+4]))
		pos += 4
	}
	if flags&trunFirstSampleFlags != 0 {
		if err := need(4); err != nil {
			return err
		}
		pos += 4
	}
	frag.DataOffset = frag.MoofOffset + int64(dataOffset)
	frag.Samples = make([]media.SampleTiming, 0, count)
	var cursor int64
	for i := uint32(0); i < count; i++ {
		s := media.SampleTiming{Index: int(i), Sync: i == 0}
		if flags&trunSampleDuration != 0 {
			if err := need(4); err != nil {
				return err
			}
			s.Duration = beUint32(buf[pos : pos+4])
			pos += 4
		}
		if flags&trunSampleSize != 0 {
			if err := need(4); err != nil {
				return err
			}
			s.Size = beUint32(buf[pos : pos+4])
			pos += 4
		}
		if flags&trunSampleFlags != 0 {
			if err := need(4); err != nil {
				return err
			}
			s.Flags = beUint32(buf[pos : pos+4])
			pos += 4
			s.Sync = s.Flags&nonSyncSampleFlag == 0
		}
		if flags&trunSampleCompositionOff != 0 {
			if err := need(4); err != nil {
				return err
			}
			if version == 1 {
				s.CompositionOffset = int64(int32(beUint32(buf[pos : pos+4])))
			} else {
				s.CompositionOffset = int64(beUint32(buf[pos : pos+4]))
			}
			pos += 4
		}
		s.PayloadOffset = frag.DataOffset + cursor
		cursor += int64(s.Size)
		if cursor < 0 {
			return &boxScanError{offset: boxStart, kind: "sample_size_overflow"}
		}
		frag.Samples = append(frag.Samples, s)
	}
	return nil
}

func validateFragment(frag *Fragment, timescales map[uint32]uint32, fileSize int64) error {
	if frag.Sequence == 0 {
		return media.Corrupt("scan", "missing_mfhd", frag.Path, uint32(frag.No), frag.MoofOffset, "mfhd sequence number is zero or absent")
	}
	if frag.TrackID == 0 {
		return media.Corrupt("scan", "missing_tfhd_track_id", frag.Path, frag.Sequence, frag.MoofOffset, "tfhd track id absent")
	}
	ts, ok := timescales[frag.TrackID]
	if !ok || ts == 0 {
		return media.Corrupt("scan", "unknown_track_timescale", frag.Path, frag.Sequence, frag.MoofOffset, "track %d has no init timescale", frag.TrackID)
	}
	frag.Timescale = ts
	if len(frag.Samples) == 0 {
		return media.Corrupt("scan", "empty_trun", frag.Path, frag.Sequence, frag.MoofOffset, "fragment contains no timed samples")
	}
	if frag.DataStart == 0 || frag.DataEnd == 0 {
		return media.Corrupt("scan", "missing_mdat", frag.Path, frag.Sequence, frag.MoofOffset, "fragment has no mdat")
	}
	var total uint64
	for _, s := range frag.Samples {
		total += uint64(s.Size)
		if s.PayloadOffset < frag.DataStart || s.PayloadOffset+int64(s.Size) > frag.DataEnd {
			off := s.PayloadOffset
			if off < 0 || off > fileSize {
				off = frag.DataEnd
			}
			return media.Corrupt("scan", "sample_outside_mdat", frag.Path, frag.Sequence, off,
				"sample %d range [%d,%d) outside mdat [%d,%d)", s.Index, s.PayloadOffset, s.PayloadOffset+int64(s.Size), frag.DataStart, frag.DataEnd)
		}
	}
	if int64(total) > frag.PayloadBytes {
		return media.Corrupt("scan", "trun_size_exceeds_mdat", frag.Path, frag.Sequence, frag.DataEnd,
			"trun sample bytes %d exceed mdat payload %d", total, frag.PayloadBytes)
	}
	return nil
}
