package scan

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	gomp4 "github.com/abema/go-mp4"

	"archivectl/internal/fault"
)

// Sample is one trun entry expanded with absolute timing and location.
type Sample struct {
	Index             int    `json:"index"`
	Duration          uint32 `json:"duration"`
	Size              uint32 `json:"size"`
	Flags             uint32 `json:"flags"`
	Sync              bool   `json:"sync"`
	CompositionOffset int64  `json:"composition_offset"`
	DTS               int64  `json:"dts"`
	PTS               int64  `json:"pts"`
	PayloadOffset     int64  `json:"payload_offset"`
}

// TFDTInfo locates and decodes the track fragment decode time.
type TFDTInfo struct {
	Version     uint8  `json:"version"`
	Base        uint64 `json:"base"`
	ValueOffset int64  `json:"value_offset"`
	ValueWidth  int    `json:"value_width"`
}

// Fragment is one parsed moof/traf unit.
type Fragment struct {
	TrackID           uint32   `json:"track_id"`
	MoofOffset        int64    `json:"moof_offset"`
	SequenceNumber    uint32   `json:"sequence_number"`
	HasTFDT           bool     `json:"has_tfdt"`
	TFDT              TFDTInfo `json:"tfdt"`
	DataOffset        int32    `json:"data_offset"`
	PayloadBase       int64    `json:"payload_base"`
	Samples           []Sample `json:"samples"`
	SyncSampleIndexes []int    `json:"sync_sample_indexes"`
	Truncated         bool     `json:"truncated"`
}

// Report is the scan result for one segment file.
type Report struct {
	Fragments []*Fragment   `json:"fragments"`
	Record    *fault.Record `json:"-"`
}

// ScanFile opens path and scans it.
func ScanFile(path, trackLabel string) (*Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ScanReader(f, trackLabel)
}

// ScanReader walks every top-level moof and its traf boxes. Structural
// damage is accumulated in Report.Record rather than returned as a
// fatal error; only an unwalkable stream error is returned.
func ScanReader(r io.ReadSeeker, trackLabel string) (*Report, error) {
	rec := fault.NewRecord()
	size, err := fileSize(r)
	if err != nil {
		return nil, err
	}
	rep := &Report{Record: rec}

	err = walkChildren(r, 0, size, func(top BoxHeader) error {
		if top.Type != "moof" {
			return nil
		}
		frag := &Fragment{MoofOffset: top.Offset, PayloadBase: top.Offset}
		moofStart, moofLen := top.PayloadRange()
		moofEnd := moofStart + moofLen
		if top.Size == 0 {
			moofEnd = size
		}
		if moofEnd > size {
			moofEnd = size
		}
		if err := walkChildren(r, moofStart, moofEnd, func(child BoxHeader) error {
			if child.Type == "traf" {
				return scanTraf(r, child, size, trackLabel, rec, frag)
			}
			payload, ok := readPayload(r, child, size, trackLabel, rec)
			if !ok || len(payload) < 4 {
				return nil
			}
			switch child.Type {
			case "mfhd":
				if len(payload) >= 8 {
					frag.SequenceNumber = binary.BigEndian.Uint32(payload[4:8])
				}
			case "tfdt":
				parseTFDT(payload, child, frag)
			case "trun":
				parseTRUN(payload, child, beUint24(payload[1:4]), frag, trackLabel, rec, false, 0)
			}
			return nil
		}); err != nil {
			return err
		}
		rep.Fragments = append(rep.Fragments, frag)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan: walk top-level: %w", err)
	}
	if len(rep.Fragments) == 0 && !rec.HasFatal() {
		rec.Add(fault.New("moof_missing", trackLabel, 0, fault.LevelFatal, "no moof box found"))
	}
	return rep, nil
}

func scanTraf(r io.ReadSeeker, traf BoxHeader, fileEnd int64, track string, rec *fault.Record, frag *Fragment) error {
	start, ln := traf.PayloadRange()
	end := start + ln
	if traf.Size == 0 || end > fileEnd {
		end = fileEnd
	}
	var defaultSampleFlags uint32
	var hasDefaultFlags bool
	var baseDataOffset int64
	var hasBaseDataOffset bool

	return walkChildren(r, start, end, func(h BoxHeader) error {
		payload, ok := readPayload(r, h, fileEnd, track, rec)
		if !ok || len(payload) < 4 {
			return nil
		}
		flags := beUint24(payload[1:4])
		switch h.Type {
		case "tfhd":
			if len(payload) < 8 {
				rec.Add(fault.New("tfhd_truncated", track, h.Offset, fault.LevelFatal, "short tfhd"))
				return nil
			}
			frag.TrackID = binary.BigEndian.Uint32(payload[4:8])
			pos := 8
			if flags&0x000001 != 0 && len(payload) >= pos+8 {
				baseDataOffset = int64(binary.BigEndian.Uint64(payload[pos : pos+8]))
				hasBaseDataOffset = true
				pos += 8
			}
			if flags&0x000002 != 0 && len(payload) >= pos+4 {
				pos += 4 // sample_description_index
			}
			if flags&0x000008 != 0 && len(payload) >= pos+4 {
				pos += 4 // default_sample_duration
			}
			if flags&0x000010 != 0 && len(payload) >= pos+4 {
				pos += 4 // default_sample_size
			}
			if flags&0x000020 != 0 && len(payload) >= pos+4 {
				defaultSampleFlags = binary.BigEndian.Uint32(payload[pos : pos+4])
				hasDefaultFlags = true
			}
			if flags&0x020000 != 0 {
				frag.PayloadBase = frag.MoofOffset
			}
			if hasBaseDataOffset {
				frag.PayloadBase = baseDataOffset
			}
		case "tfdt":
			parseTFDT(payload, h, frag)
		case "trun":
			parseTRUN(payload, h, flags, frag, track, rec, hasDefaultFlags, defaultSampleFlags)
		}
		return nil
	})
}

func parseTFDT(payload []byte, h BoxHeader, frag *Fragment) {
	version := payload[0]
	info := TFDTInfo{Version: version, ValueOffset: h.Offset + h.HeaderSize + 4}
	// Decode through go-mp4 for spec-compatible version handling.
	var tfdt gomp4.Tfdt
	if n, err := gomp4.Unmarshal(bytes.NewReader(payload), uint64(len(payload)), &tfdt, gomp4.Context{}); err == nil && n > 0 {
		info.Base = tfdt.GetBaseMediaDecodeTime()
	} else if version == 1 && len(payload) >= 12 {
		info.Base = binary.BigEndian.Uint64(payload[4:12])
	} else if len(payload) >= 8 {
		info.Base = uint64(binary.BigEndian.Uint32(payload[4:8]))
	}
	if version == 1 {
		info.ValueWidth = 8
	} else {
		info.ValueWidth = 4
	}
	frag.HasTFDT = true
	frag.TFDT = info
}

func parseTRUN(payload []byte, h BoxHeader, flags uint32, frag *Fragment, track string, rec *fault.Record, hasDefaultFlags bool, defaultFlags uint32) {
	if len(payload) < 8 {
		rec.Add(fault.New("trun_truncated", track, h.Offset, fault.LevelFatal, "trun shorter than header"))
		frag.Truncated = true
		return
	}
	count := binary.BigEndian.Uint32(payload[4:8])
	pos := 8
	var dataOffset int32
	if flags&0x000001 != 0 {
		if len(payload) < pos+4 {
			rec.Add(fault.New("trun_truncated", track, h.Offset, fault.LevelFatal, "missing data_offset"))
			frag.Truncated = true
			return
		}
		dataOffset = int32(binary.BigEndian.Uint32(payload[pos : pos+4]))
		frag.DataOffset = dataOffset
		pos += 4
	}
	var firstFlags uint32
	hasFirstFlags := flags&0x000004 != 0
	if hasFirstFlags {
		if len(payload) < pos+4 {
			rec.Add(fault.New("trun_truncated", track, h.Offset, fault.LevelFatal, "missing first_sample_flags"))
			frag.Truncated = true
			return
		}
		firstFlags = binary.BigEndian.Uint32(payload[pos : pos+4])
		pos += 4
	}

	perSample := 0
	if flags&0x000100 != 0 {
		perSample += 4
	}
	if flags&0x000200 != 0 {
		perSample += 4
	}
	if flags&0x000400 != 0 {
		perSample += 4
	}
	if flags&0x000800 != 0 {
		perSample += 4
	}
	need := pos + int(count)*perSample
	if perSample == 0 {
		need = pos
	}
	if int(count) > 0 && len(payload) < need {
		rec.Add(fault.New("trun_truncated", track, h.Offset, fault.LevelFatal,
			fmt.Sprintf("trun sample_count=%d needs %d payload bytes but have %d", count, need, len(payload))))
		frag.Truncated = true
		return
	}

	// Use go-mp4 to decode the intact entry table.
	var trun gomp4.Trun
	if _, err := gomp4.Unmarshal(bytes.NewReader(payload), uint64(len(payload)), &trun, gomp4.Context{}); err != nil {
		rec.Add(fault.New("trun_decode_failed", track, h.Offset, fault.LevelFatal, err.Error()))
		frag.Truncated = true
		return
	}

	base := int64(frag.TFDT.Base)
	var cumDuration int64
	payloadCursor := frag.PayloadBase + int64(dataOffset)
	for i := 0; i < int(count); i++ {
		e := trun.Entries[i]
		s := Sample{
			Index:    i,
			Duration: e.SampleDuration,
			Size:     e.SampleSize,
			Flags:    e.SampleFlags,
			DTS:      base + cumDuration,
		}
		if flags&0x000800 != 0 {
			if trun.GetVersion() == 1 {
				s.CompositionOffset = int64(e.SampleCompositionTimeOffsetV1)
			} else {
				s.CompositionOffset = int64(e.SampleCompositionTimeOffsetV0)
			}
		}
		s.PTS = s.DTS + s.CompositionOffset
		s.PayloadOffset = payloadCursor
		payloadCursor += int64(e.SampleSize)

		effectiveFlags := e.SampleFlags
		switch {
		case i == 0 && hasFirstFlags:
			effectiveFlags = firstFlags
		case flags&0x000400 == 0 && hasDefaultFlags:
			effectiveFlags = defaultFlags
		}
		// sample_is_non_sync_sample is bit 0x00010000; clear => sync.
		s.Sync = effectiveFlags&0x00010000 == 0
		if s.Sync {
			frag.SyncSampleIndexes = append(frag.SyncSampleIndexes, i)
		}
		frag.Samples = append(frag.Samples, s)
		cumDuration += int64(e.SampleDuration)
	}
}
