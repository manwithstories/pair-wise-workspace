package verify

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"

	"archivecheck/internal/fault"
	"archivecheck/internal/index"
)

// Record is one fragment-track block recovered from the output container.
type Record struct {
	Segment      string
	TrackID      uint32
	Sequence     uint32
	Timescale    uint32
	OriginalBase uint64
	RepairedBase uint64
	Reason       byte
	Samples      []SampleRecord
}

// SampleRecord is one sample recovered from the output container.
type SampleRecord struct {
	DTS      uint64
	EndDTS   uint64
	Duration uint32
	Size     uint32
	Flags    uint32
	CTO      int32
	Offset   int64
}

// Report is the independent verification result.
type Report struct {
	Verified             bool
	Fragments            int
	Entries              int
	TimestampRegressions int
	InvalidIndexOffsets  int
	OutputBytes          int64
	OutputSHA256         string
	ContentDigest        string
	Reasons              []string
}

// Scan is a fully decoded output container.
type Scan struct {
	Records       []Record
	Entries       []index.Entry
	ContentDigest string
	OutputSHA256  string
	ContentBytes  int64
}

// ScanFile decodes the container at path up to limit bytes. A limit <= 0
// scans the entire file. Partial (interrupted) containers have no trailer and
// therefore yield no ContentDigest.
func ScanFile(path string, limit int64) (*Scan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fault.Wrap(err, fault.KindOutput, path, "", 0, "cannot read output")
	}
	if limit > 0 {
		if limit > int64(len(data)) {
			return nil, fault.New(fault.KindOutput, path, "", int64(len(data)),
				fmt.Sprintf("output is shorter than committed %d bytes", limit))
		}
		data = data[:limit]
	}
	return scanBytes(data, path, limit <= 0)
}

func scanBytes(data []byte, path string, expectTrailer bool) (*Scan, error) {
	if len(data) < len(Magic) || !bytes.Equal(data[:len(Magic)], Magic[:]) {
		return nil, fault.New(fault.KindOutput, path, "", 0, "bad container magic")
	}
	s := &Scan{}
	pos := len(Magic)
	complete := false
	for pos < len(data) {
		tag := data[pos]
		switch tag {
		case tagFragment:
			rec, next, err := scanRecord(data, pos+1, path)
			if err != nil {
				return nil, err
			}
			s.Records = append(s.Records, rec)
			for _, smp := range rec.Samples {
				s.Entries = append(s.Entries, index.Entry{
					Segment: rec.Segment, Sequence: rec.Sequence, TrackID: rec.TrackID,
					Timescale: rec.Timescale, DTS: smp.DTS, EndDTS: smp.EndDTS,
					Offset: smp.Offset, Size: smp.Size, Sync: smp.Flags&0x00010000 == 0,
				})
			}
			pos = next
		case tagEnd:
			complete = true
			pos++
			if !expectTrailer {
				// The caller deliberately limited the scan to a committed
				// prefix; a trailer inside that prefix is still validated.
				_ = complete
			}
			if len(data)-pos != sha256.Size {
				return nil, fault.New(fault.KindOutput, path, "", int64(pos),
					fmt.Sprintf("trailer length %d, want %d", len(data)-pos, sha256.Size))
			}
			sum := sha256.Sum256(data[:pos])
			if !bytes.Equal(sum[:], data[pos:]) {
				return nil, fault.New(fault.KindOutput, path, "", int64(pos), "content digest mismatch")
			}
			s.ContentDigest = hex.EncodeToString(sum[:])
			s.ContentBytes = int64(pos)
			pos = len(data)
		default:
			return nil, fault.New(fault.KindOutput, path, "", int64(pos),
				fmt.Sprintf("unknown record tag 0x%02x", tag))
		}
	}
	if expectTrailer && !complete {
		return nil, fault.New(fault.KindOutput, path, "", int64(len(data)), "output has no end marker")
	}
	if !expectTrailer {
		s.ContentBytes = int64(len(data))
	}
	full := sha256.Sum256(data)
	s.OutputSHA256 = hex.EncodeToString(full[:])
	return s, nil
}

func scanRecord(data []byte, pos int, path string) (Record, int, error) {
	var rec Record
	start := pos
	need := func(n int) error {
		if pos+n > len(data) {
			return fault.New(fault.KindShortRead, path, "", int64(pos),
				fmt.Sprintf("truncated record at %d needing %d bytes", start, n))
		}
		return nil
	}
	if err := need(2); err != nil {
		return rec, 0, err
	}
	nameLen := int(binary.BigEndian.Uint16(data[pos:]))
	pos += 2
	if err := need(nameLen); err != nil {
		return rec, 0, err
	}
	rec.Segment = string(data[pos : pos+nameLen])
	pos += nameLen
	if err := need(4 * 3); err != nil {
		return rec, 0, err
	}
	rec.TrackID = binary.BigEndian.Uint32(data[pos:])
	rec.Sequence = binary.BigEndian.Uint32(data[pos+4:])
	rec.Timescale = binary.BigEndian.Uint32(data[pos+8:])
	pos += 12
	if err := need(8 * 2); err != nil {
		return rec, 0, err
	}
	rec.OriginalBase = binary.BigEndian.Uint64(data[pos:])
	rec.RepairedBase = binary.BigEndian.Uint64(data[pos+8:])
	pos += 16
	if err := need(1 + 4); err != nil {
		return rec, 0, err
	}
	rec.Reason = data[pos]
	pos++
	count := binary.BigEndian.Uint32(data[pos:])
	pos += 4
	if count > 1<<20 {
		return rec, 0, fault.New(fault.KindInvalidSize, path, "", int64(pos), "implausible sample count")
	}
	rec.Samples = make([]SampleRecord, 0, count)
	for i := uint32(0); i < count; i++ {
		if err := need(32); err != nil {
			return rec, 0, err
		}
		smp := SampleRecord{
			DTS:      binary.BigEndian.Uint64(data[pos:]),
			EndDTS:   binary.BigEndian.Uint64(data[pos+8:]),
			Duration: binary.BigEndian.Uint32(data[pos+16:]),
			Size:     binary.BigEndian.Uint32(data[pos+20:]),
			Flags:    binary.BigEndian.Uint32(data[pos+24:]),
			CTO:      int32(binary.BigEndian.Uint32(data[pos+28:])),
		}
		pos += 32
		if err := need(int(smp.Size)); err != nil {
			return rec, 0, err
		}
		smp.Offset = int64(pos)
		pos += int(smp.Size)
		rec.Samples = append(rec.Samples, smp)
	}
	return rec, pos, nil
}

// Check independently re-reads the completed output and compares it with the
// committed checkpoint. Timestamp monotonicity, sample boundaries and index
// offsets are all recalculated from the bytes on disk.
func Check(path string, cp *index.Checkpoint) (*Report, error) {
	rep := &Report{}
	add := func(format string, args ...any) { rep.Reasons = append(rep.Reasons, fmt.Sprintf(format, args...)) }
	scan, err := ScanFile(path, 0)
	if err != nil {
		rep.Reasons = append(rep.Reasons, err.Error())
		if fe, ok := fault.As(err); ok {
			rep.InvalidIndexOffsets++
			_ = fe
		}
		return rep, err
	}
	rep.Fragments = len(scan.Records)
	rep.Entries = len(scan.Entries)
	rep.OutputBytes = int64(fileSize(scan))
	rep.OutputSHA256 = scan.OutputSHA256
	rep.ContentDigest = scan.ContentDigest
	if cp == nil {
		add("missing checkpoint")
		return rep, nil
	}
	if cp.CommittedDigest != scan.ContentDigest {
		add("checkpoint committed digest %s != output content digest %s", cp.CommittedDigest, scan.ContentDigest)
	}
	if cp.OutputBytes != scan.ContentBytes {
		add("checkpoint output bytes %d != content bytes %d", cp.OutputBytes, scan.ContentBytes)
	}

	payloadOffsets := make(map[int64]uint32, len(scan.Entries))
	for _, e := range scan.Entries {
		payloadOffsets[e.Offset] = e.Size
	}
	lastEnd := map[uint32]uint64{}
	for _, e := range scan.Entries {
		if prev, ok := lastEnd[e.TrackID]; ok && e.DTS < prev {
			rep.TimestampRegressions++
		}
		lastEnd[e.TrackID] = e.EndDTS
		if e.EndDTS <= e.DTS {
			add("sample at offset %d has non-positive duration", e.Offset)
		}
		if e.Offset < 0 || e.Size == 0 || e.Offset+int64(e.Size) > scan.ContentBytes {
			rep.InvalidIndexOffsets++
		}
	}
	if cp.Index != nil {
		norm, err := index.Normalize(scan.Entries)
		if err != nil {
			add("cannot normalize recovered index: %v", err)
		} else if len(norm.Entries) != len(cp.Index.Entries) {
			add("checkpoint index has %d entries, output has %d", len(cp.Index.Entries), len(norm.Entries))
		} else {
			for i := range norm.Entries {
				want := cp.Index.Entries[i]
				if want.Offset != norm.Entries[i].Offset || want.Size != norm.Entries[i].Size ||
					want.DTS != norm.Entries[i].DTS || want.TrackID != norm.Entries[i].TrackID {
					add("checkpoint index entry %d does not match output", i)
					rep.InvalidIndexOffsets++
					break
				}
			}
		}
		for _, e := range cp.Index.Entries {
			size, ok := payloadOffsets[e.Offset]
			if !ok || size != e.Size {
				rep.InvalidIndexOffsets++
			}
		}
	}
	rep.Verified = len(rep.Reasons) == 0 && rep.TimestampRegressions == 0 && rep.InvalidIndexOffsets == 0
	return rep, nil
}

func fileSize(s *Scan) int64 {
	if s.ContentDigest != "" {
		return s.ContentBytes + sha256.Size
	}
	return s.ContentBytes
}
