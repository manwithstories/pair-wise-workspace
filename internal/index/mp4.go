// Package index rebuilds a random-access index into a fragmented or
// progressive ISO-BMFF file and verifies that every index point lands on a
// valid sample boundary. The index time is reconciled against the clock repair
// mapping so that a seek in the output matches the repaired timeline.
package index

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/abema/go-mp4"
)

// Sentinel errors.
var (
	ErrTruncated    = errors.New("index source is truncated")
	ErrNoSamples    = errors.New("no samples found in source")
	ErrBadBoundary  = errors.New("index point is not on a sample boundary")
	ErrTimeMismatch = errors.New("index time disagrees with repair mapping")
	ErrUnsupported  = errors.New("unsupported or corrupt box structure")
)

// Point is one random-access index entry.
type Point struct {
	TimeNs     int64 // decode time on the shared base
	DurationNs int64
	Offset     int64 // byte offset of the first sample
	Size       int64 // total bytes of the indexed run
	Sync       bool  // samples in this run are key frames
	SegmentID  string
}

// Track describes one indexed track.
type Track struct {
	ID        uint32
	Timescale uint32
	Handler   string
	Points    []Point
	Samples   int
}

// Index is the rebuilt random-access index.
type Index struct {
	Tracks     []Track
	MOOVOffset int64
	Truncated  bool
	Warnings   []string
}

// Report is the verification outcome.
type Report struct {
	Points     int
	SyncPoints int
	Valid      bool
	Mismatches int
	Problems   []string
}

// source is the parsed media file: either progressive or fragmented.
type source struct {
	file       *os.File
	size       int64
	tracks     []trackSource
	fragments  []fragment
	fragmented bool
	warnings   []string
	truncated  bool
}

type trackSource struct {
	id        uint32
	timescale uint32
	handler   string
	samples   []sampleRef
}

type sampleRef struct {
	offset int64
	size   int64
	timeNs int64
	durNs  int64
	sync   bool
}

type fragment struct {
	trackID uint32
	seq     uint32
	baseNs  int64
	endNs   int64
	offset  int64
	size    int64
	trunc   bool
}

// Open parses the file structure. A truncated final box is reported rather
// than treated as a fatal error so that the caller can quarantine the tail.
func Open(path string) (*source, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	src := &source{file: f, size: st.Size()}
	if err := src.parse(); err != nil {
		f.Close()
		return nil, err
	}
	return src, nil
}

// Close releases the file handle.
func (s *source) Close() error { return s.file.Close() }

// parse walks the top-level boxes, then the sample tables of moov and any
// moof/traf fragments.
func (s *source) parse() error {
	var moov *mp4.BoxInfo
	var mdatRanges [][2]int64
	var moofs []*mp4.BoxInfo
	off := int64(0)
	for off < s.size {
		bi, hasData, err := readHeader(s.file, off, s.size)
		if err != nil {
			s.truncated = true
			s.warnings = append(s.warnings, fmt.Sprintf("truncated box at offset %d: %v", off, err))
			break
		}
		end := off + int64(bi.Size)
		if end > s.size {
			s.truncated = true
			s.warnings = append(s.warnings, fmt.Sprintf("box %s at %d extends past EOF", bi.Type, off))
			end = s.size
		}
		if bi.Type == mp4.BoxTypeMoov() {
			moov = bi
		}
		switch bi.Type {
		case mp4.BoxTypeMoof():
			moofs = append(moofs, bi)
		case mp4.BoxTypeMdat():
			if hasData {
				start := off + int64(bi.HeaderSize)
				mdatRanges = append(mdatRanges, [2]int64{start, end})
			}
		}
		off = end
	}
	if moov == nil {
		return fmt.Errorf("%w: moov box not found", ErrUnsupported)
	}
	if err := s.parseMoov(moov, mdatRanges); err != nil {
		return err
	}
	if len(moofs) > 0 {
		s.fragmented = true
		for _, mf := range moofs {
			if err := s.parseMoof(mf); err != nil {
				s.warnings = append(s.warnings, err.Error())
			}
		}
	}
	if len(s.tracks) == 0 {
		return ErrNoSamples
	}
	return nil
}

// readHeader reads a box header at offset without materialising the payload.
func readHeader(f io.ReadSeeker, off, size int64) (*mp4.BoxInfo, bool, error) {
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, false, err
	}
	bi, err := mp4.ReadBoxInfo(f)
	if err != nil {
		return nil, false, err
	}
	if bi.Size < bi.HeaderSize {
		return nil, false, fmt.Errorf("box %s has size %d < header %d", bi.Type, bi.Size, bi.HeaderSize)
	}
	hasData := bi.Type != mp4.BoxTypeMoof() && bi.Type != mp4.BoxTypeMoov() &&
		bi.Type != mp4.BoxTypeMfra() && bi.Type != mp4.BoxTypeFtyp() && bi.Type != mp4.BoxTypeStyp()
	return bi, hasData, nil
}

func (s *source) boxAt(off int64) (*mp4.BoxInfo, error) {
	if _, err := s.file.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	return mp4.ReadBoxInfo(s.file)
}

// extractBoxes lists boxes at a path inside parent.
func (s *source) extractBoxes(parent *mp4.BoxInfo, path ...mp4.BoxType) ([]*mp4.BoxInfo, error) {
	return mp4.ExtractBoxes(s.file, parent, []mp4.BoxPath{path})
}

func (s *source) payload(parent *mp4.BoxInfo, boxType mp4.BoxType, path ...mp4.BoxType) (mp4.IBox, error) {
	p := append(append([]mp4.BoxType{}, path...), boxType)
	bips, err := mp4.ExtractBoxesWithPayload(s.file, parent, []mp4.BoxPath{p})
	if err != nil {
		return nil, err
	}
	if len(bips) == 0 {
		return nil, nil
	}
	return bips[0].Payload, nil
}
