package capture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pion/rtp"

	"mediad/internal/fault"
)

const IndexVersion = 1

var (
	ErrIndexMissing    = errors.New("capture index missing")
	ErrIndexCorrupt    = errors.New("capture index checksum or encoding is corrupt")
	ErrIndexOutOfRange = errors.New("capture index offset is out of bounds")
)

// IndexEntry is one sparse seek point into the framed capture.
type IndexEntry struct {
	Index     int    `json:"index"`
	Kind      uint8  `json:"kind"`
	Offset    int64  `json:"offset"`
	FrameSize int    `json:"frame_size"`
	ArrivalNS int64  `json:"arrival_ns"`
	Sequence  uint16 `json:"sequence,omitempty"`
	Timestamp uint32 `json:"timestamp,omitempty"`
	CRCValid  bool   `json:"crc_valid"`
	DamageAt  int64  `json:"damage_at,omitempty"`
}

// Index is a checksummed sparse capture index.
type Index struct {
	Version     int          `json:"version"`
	FileSize    int64        `json:"file_size"`
	EndOffset   int64        `json:"end_offset"`
	Interval    int          `json:"interval"`
	RecordCount int          `json:"record_count"`
	Entries     []IndexEntry `json:"entries"`
	SHA256      string       `json:"sha256"`
}

type indexDigest struct {
	Version     int          `json:"version"`
	FileSize    int64        `json:"file_size"`
	EndOffset   int64        `json:"end_offset"`
	Interval    int          `json:"interval"`
	RecordCount int          `json:"record_count"`
	Entries     []IndexEntry `json:"entries"`
}

func (idx *Index) digest() (string, error) {
	b, err := json.Marshal(indexDigest{
		Version: idx.Version, FileSize: idx.FileSize, EndOffset: idx.EndOffset,
		Interval: idx.Interval, RecordCount: idx.RecordCount, Entries: idx.Entries,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Validate verifies the checksum and every sparse byte range before use.
func (idx *Index) Validate(actualSize int64) error {
	if idx == nil {
		return ErrIndexMissing
	}
	if idx.Version != IndexVersion {
		return fmt.Errorf("%w: version %d", ErrIndexCorrupt, idx.Version)
	}
	if actualSize >= 0 && idx.FileSize != actualSize {
		return fmt.Errorf("%w: index size %d != capture size %d", ErrIndexCorrupt, idx.FileSize, actualSize)
	}
	want, err := idx.digest()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrIndexCorrupt, err)
	}
	if idx.SHA256 != want {
		return fmt.Errorf("%w: checksum mismatch", ErrIndexCorrupt)
	}
	if idx.Interval <= 0 {
		return fmt.Errorf("%w: invalid interval %d", ErrIndexCorrupt, idx.Interval)
	}
	if idx.EndOffset < 0 || idx.EndOffset > idx.FileSize {
		return fmt.Errorf("%w: end offset %d outside size %d", ErrIndexOutOfRange, idx.EndOffset, idx.FileSize)
	}

	var lastOffset int64 = -1
	for i, e := range idx.Entries {
		if e.Offset < 0 || e.FrameSize < headerSize+crcSize {
			return fmt.Errorf("%w: entry %d has invalid range", ErrIndexOutOfRange, i)
		}
		end, err := fault.SafeAddOffset(e.Offset, e.FrameSize)
		if err != nil || end > idx.FileSize {
			return fmt.Errorf("%w: entry %d offset=%d size=%d", ErrIndexOutOfRange, i, e.Offset, e.FrameSize)
		}
		if e.Offset <= lastOffset {
			return fmt.Errorf("%w: entry %d is not sparse/ordered", ErrIndexCorrupt, i)
		}
		if i > 0 {
			prev := idx.Entries[i-1]
			prevEnd, _ := fault.SafeAddOffset(prev.Offset, prev.FrameSize)
			if e.Offset < prevEnd {
				return fmt.Errorf("%w: entry %d overlaps previous frame", ErrIndexOutOfRange, i)
			}
		}
		lastOffset = e.Offset
	}
	return nil
}

// LoadIndex reads and validates an index file. A missing file is reported with
// ErrIndexMissing; a short/corrupt file is ErrIndexCorrupt.
func LoadIndex(path string, actualSize int64) (*Index, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrIndexMissing
	}
	if err != nil {
		return nil, fmt.Errorf("read index: %w", err)
	}
	var idx Index
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIndexCorrupt, err)
	}
	if err := idx.Validate(actualSize); err != nil {
		return nil, err
	}
	return &idx, nil
}

// WriteIndex atomically writes the checksummed sparse index.
func WriteIndex(path string, idx *Index) error {
	sum, err := idx.digest()
	if err != nil {
		return err
	}
	idx.SHA256 = sum
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// BuildIndex scans a capture and creates a sparse index every interval records.
func BuildIndex(r io.Reader, fileSize int64, interval int) (*Index, error) {
	if interval <= 0 {
		interval = 32
	}
	sc, err := NewScanner(r, fileSize)
	if err != nil {
		return nil, err
	}
	idx := &Index{Version: IndexVersion, FileSize: fileSize, Interval: interval}
	recordNo := 0
	for {
		rec, err := sc.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		frameSize := headerSize + 1 + 8 + len(rec.Packet) + crcSize
		if !rec.CRCValid {
			frameSize = int(binaryLengthFromHeader(rec.HeaderBytes)) + headerSize
		}
		idx.RecordCount++
		if recordNo%interval == 0 {
			e := IndexEntry{
				Index: recordNo, Kind: rec.Kind, Offset: rec.Offset, FrameSize: frameSize,
				ArrivalNS: rec.ArrivalNS, CRCValid: rec.CRCValid, DamageAt: rec.DamageOffset,
			}
			if rec.Kind == KindRTP && rec.CRCValid {
				var pkt rtp.Packet
				if err := pkt.Unmarshal(rec.Packet); err == nil {
					e.Sequence = pkt.SequenceNumber
					e.Timestamp = pkt.Timestamp
				}
			}
			idx.Entries = append(idx.Entries, e)
		}
		recordNo++
		idx.EndOffset = sc.Offset()
	}
	return idx, nil
}

func binaryLengthFromHeader(h [headerSize]byte) uint32 {
	return uint32(h[4])<<24 | uint32(h[5])<<16 | uint32(h[6])<<8 | uint32(h[7])
}

// LoadOrRebuild uses a valid sparse index when possible and scans otherwise.
func LoadOrRebuild(indexPath, capturePath string, interval int) (*Index, bool, error) {
	st, err := os.Stat(capturePath)
	if err != nil {
		return nil, false, fault.Usage("index", err)
	}
	if err := fault.CheckInputSize(st.Size()); err != nil {
		return nil, false, err
	}
	if idx, err := LoadIndex(indexPath, st.Size()); err == nil {
		return idx, false, nil
	} else if !errors.Is(err, ErrIndexMissing) && !errors.Is(err, ErrIndexCorrupt) && !errors.Is(err, ErrIndexOutOfRange) {
		return nil, false, err
	}
	f, err := os.Open(capturePath)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	idx, err := BuildIndex(f, st.Size(), interval)
	if err != nil {
		return nil, true, err
	}
	if err := WriteIndex(indexPath, idx); err != nil {
		return idx, true, fmt.Errorf("rebuilt index but write failed: %w", err)
	}
	return idx, true, nil
}
