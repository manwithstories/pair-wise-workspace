// Package mp4 provides a bounded, streaming reader for ISO base media files
// and a deterministic writer that repackages samples into standalone segment
// files. The reader never buffers a whole file: box sizes are validated
// against a limit before any payload is read, and sample payloads are read
// only on demand by the caller.
package mp4

import (
	"fmt"
	"os"

	gomp4 "github.com/abema/go-mp4"

	"medialine/internal/fault"
	"medialine/internal/model"
)

// Default limits keep peak memory bounded independently of file size.
const (
	DefaultMaxBoxSize  = 64 << 20 // 64 MiB
	DefaultReadBuffer  = 256 << 10
	DefaultMaxInflight = 8
)

// Limits bounds every allocation the reader performs.
type Limits struct {
	MaxBoxSize  uint64
	ReadBuffer  int
	MaxInflight int
}

// Normalised fills zero fields with defaults.
func (l Limits) Normalised() Limits {
	if l.MaxBoxSize == 0 {
		l.MaxBoxSize = DefaultMaxBoxSize
	}
	if l.ReadBuffer <= 0 {
		l.ReadBuffer = DefaultReadBuffer
	}
	if l.MaxInflight <= 0 {
		l.MaxInflight = DefaultMaxInflight
	}
	return l
}

// Span is a byte range inside a source file.
type Span struct {
	Offset uint64
	Size   uint64
}

// End returns the exclusive end offset of the span.
func (s Span) End() uint64 { return s.Offset + s.Size }

// Options configures parsing and fixture integrity checking.
type Options struct {
	Limits Limits
	// CheckSampleMarker validates the medialine fixture marker embedded in
	// each sample payload. Real-world files should leave this off.
	CheckSampleMarker bool
}

// File is the parsed representation of one source MP4.
type File struct {
	Path     string
	Size     int64
	Ftyp     gomp4.Ftyp
	Mdat     Span
	Moov     Span
	Mfra     Span
	HasMoov  bool
	Tracks   []*model.Track
	Issues   []*fault.Error
	Limits   Limits
	marker   bool
	resynced bool
}

// Close releases the underlying handle. Parse closes its own file handle, so
// this exists to satisfy resource-management interfaces in callers.
func (f *File) Close() error { return nil }

// AddIssue records a recoverable fault.
func (f *File) AddIssue(e *fault.Error) {
	if e != nil {
		f.Issues = append(f.Issues, e)
	}
}

// Parse reads the box structure of path, building tracks from the sample
// tables. Structural faults that prevent finding a useful moov are returned
// as errors; per-track faults are collected in File.Issues so callers can keep
// processing intact tracks.
func Parse(path string, opts Options) (*File, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, fault.From(err, fault.StageParse, fault.KindMissingIndex)
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return nil, fault.From(err, fault.StageParse, fault.KindMissingIndex)
	}
	f := &File{Path: path, Size: st.Size(), Limits: opts.Limits.Normalised(), marker: opts.CheckSampleMarker}

	if err := f.scanTopLevel(fh); err != nil {
		return f, err
	}
	if !f.HasMoov {
		f.AddIssue(fault.New(fault.StageIndex, fault.KindMissingIndex,
			"no moov box found before end of file").At(fault.UnknownTrack, f.Size))
		return f, nil
	}
	f.parseMoov(fh)
	if e := f.checkMfra(fh); e != nil {
		f.AddIssue(e)
	}
	return f, nil
}

// scanTopLevel walks root boxes, validating sizes before each seek. It records
// the mdat/moov/mfra spans without reading payloads.
func (f *File) scanTopLevel(fh *os.File) error {
	var off uint64
	for off+8 <= uint64(f.Size) {
		bi, err := childBox(fh, off, 0, uint64(f.Size), f.Limits.MaxBoxSize)
		if err != nil {
			fe, _ := fault.As(err)
			// A corrupt declaration is recoverable when a later box header can
			// be found: record the fault and resynchronise. This is what lets
			// a delivery with one damaged box still yield its intact tracks.
			if fe != nil && fe.Kind == fault.KindInvalidLength {
				f.AddIssue(fe)
				next, ok := resyncBox(fh, off+8, uint64(f.Size))
				if ok {
					f.resynced = true
					off = next
					continue
				}
			}
			if fe != nil && f.HasMoov && f.Mdat.Size > 0 {
				// A corrupt trailing region is tolerable once mdat and moov
				// are complete: record it and stop scanning.
				f.AddIssue(fe)
				if fe.Kind == fault.KindTruncatedBox {
					f.AddIssue(fault.New(fault.StageIndex, fault.KindMissingIndex,
						"tail truncated: trailing index unavailable").At(fault.UnknownTrack, fe.Offset))
				}
				return nil
			}
			return err
		}
		switch bi.Type {
		case gomp4.BoxTypeFtyp():
			if err := f.readFtyp(fh, bi); err != nil {
				return err
			}
		case gomp4.BoxTypeMdat():
			f.Mdat = Span{Offset: bi.Offset, Size: bi.Size}
		case gomp4.BoxTypeMoov():
			f.Moov = Span{Offset: bi.Offset, Size: bi.Size}
			f.HasMoov = true
		case gomp4.BoxTypeMfra():
			f.Mfra = Span{Offset: bi.Offset, Size: bi.Size}
		default:
			// An unknown top-level box is only suspicious before the first
			// resync: after that, garbage from a corrupt length is expected.
			if !f.HasMoov && !f.resynced && bi.Type != gomp4.BoxTypeFree() {
				f.AddIssue(fault.New(fault.StageParse, fault.KindInvalidLength,
					"box %s at %d is not part of an ISO base media file", bi.Type, bi.Offset).
					At(fault.UnknownTrack, int64(bi.Offset)))
			}
		}
		off = bi.Offset + bi.Size
	}
	if off < uint64(f.Size) {
		f.AddIssue(fault.New(fault.StageParse, fault.KindTruncatedBox,
			"%d trailing bytes do not form a box", uint64(f.Size)-off).At(fault.UnknownTrack, int64(off)))
	}
	return nil
}

func (f *File) readFtyp(fh *os.File, bi *gomp4.BoxInfo) error {
	return readBoxPayload(fh, bi, &f.Ftyp)
}

// parseMoov walks the moov tree one trak at a time so that a damaged track
// does not discard its siblings.
func (f *File) parseMoov(fh *os.File) {
	children, err := childSpans(fh, f.Moov.Offset+8, f.Moov.End(), f.Limits.MaxBoxSize)
	if err != nil {
		f.AddIssue(fault.From(err, fault.StageParse, fault.KindCorruptSample).
			At(fault.UnknownTrack, int64(f.Moov.Offset)))
	}
	for _, bi := range children {
		if bi.Type != gomp4.BoxTypeTrak() {
			continue
		}
		tb := &trakBoxes{offset: bi.Offset}
		if err := f.readTrak(fh, bi, tb); err != nil {
			fe := fault.From(err, fault.StageParse, fault.KindCorruptSample)
			fe.TrackID = tb.trackID()
			f.AddIssue(fe)
			continue
		}
		tr, issues := buildTrack(tb, f.Path)
		for _, iss := range issues {
			f.AddIssue(iss)
		}
		if tr != nil {
			f.Tracks = append(f.Tracks, tr)
		}
	}
}

// readTrak fills tb from the direct children of one trak box.
func (f *File) readTrak(fh *os.File, trakBox *gomp4.BoxInfo, tb *trakBoxes) error {
	kids, err := childSpans(fh, trakBox.Offset+8, trakBox.Offset+trakBox.Size, f.Limits.MaxBoxSize)
	if err != nil {
		return err
	}
	for _, bi := range kids {
		switch bi.Type {
		case gomp4.BoxTypeTkhd():
			tb.tkhd = &gomp4.Tkhd{}
			if err := readBoxPayload(fh, bi, tb.tkhd); err != nil {
				return err
			}
		case gomp4.BoxTypeEdts():
			if err := f.readEdts(fh, bi, tb); err != nil {
				return err
			}
		case gomp4.BoxTypeMdia():
			if err := f.readMdia(fh, bi, tb); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *File) readEdts(fh *os.File, edts *gomp4.BoxInfo, tb *trakBoxes) error {
	kids, err := childSpans(fh, edts.Offset+8, edts.Offset+edts.Size, f.Limits.MaxBoxSize)
	if err != nil {
		return err
	}
	for _, bi := range kids {
		if bi.Type == gomp4.BoxTypeElst() {
			tb.elst = &gomp4.Elst{}
			return readBoxPayload(fh, bi, tb.elst)
		}
	}
	return nil
}

func (f *File) readMdia(fh *os.File, mdia *gomp4.BoxInfo, tb *trakBoxes) error {
	kids, err := childSpans(fh, mdia.Offset+8, mdia.Offset+mdia.Size, f.Limits.MaxBoxSize)
	if err != nil {
		return err
	}
	for _, bi := range kids {
		switch bi.Type {
		case gomp4.BoxTypeMdhd():
			tb.mdhd = &gomp4.Mdhd{}
			if err := readBoxPayload(fh, bi, tb.mdhd); err != nil {
				return err
			}
		case gomp4.BoxTypeHdlr():
			tb.hdlr = &gomp4.Hdlr{}
			if err := readBoxPayload(fh, bi, tb.hdlr); err != nil {
				return err
			}
		case gomp4.BoxTypeMinf():
			if err := f.readMinf(fh, bi, tb); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *File) readMinf(fh *os.File, minf *gomp4.BoxInfo, tb *trakBoxes) error {
	kids, err := childSpans(fh, minf.Offset+8, minf.Offset+minf.Size, f.Limits.MaxBoxSize)
	if err != nil {
		return err
	}
	for _, bi := range kids {
		if bi.Type == gomp4.BoxTypeStbl() {
			return f.readStbl(fh, bi, tb)
		}
	}
	return nil
}

// readStbl collects the sample-table boxes. Each box is read in isolation: a
// broken stss only costs keyframe precision, while a broken stsz/stts/stco is
// detected later by buildTrack.
func (f *File) readStbl(fh *os.File, stbl *gomp4.BoxInfo, tb *trakBoxes) error {
	kids, err := childSpans(fh, stbl.Offset+8, stbl.Offset+stbl.Size, f.Limits.MaxBoxSize)
	if err != nil {
		f.AddIssue(fault.From(err, fault.StageIndex, fault.KindCorruptSample).
			At(tb.trackID(), int64(stbl.Offset)))
	}
	for _, bi := range kids {
		var derr error
		switch bi.Type {
		case gomp4.BoxTypeStts():
			tb.stts = &gomp4.Stts{}
			derr = readBoxPayload(fh, bi, tb.stts)
		case gomp4.BoxTypeStsc():
			tb.stsc = &gomp4.Stsc{}
			derr = readBoxPayload(fh, bi, tb.stsc)
		case gomp4.BoxTypeStsz():
			tb.stsz = &gomp4.Stsz{}
			derr = readBoxPayload(fh, bi, tb.stsz)
		case gomp4.BoxTypeStco():
			tb.stco = &gomp4.Stco{}
			derr = readBoxPayload(fh, bi, tb.stco)
		case gomp4.BoxTypeCo64():
			tb.co64 = &gomp4.Co64{}
			derr = readBoxPayload(fh, bi, tb.co64)
		case gomp4.BoxTypeStss():
			tb.stss = &gomp4.Stss{}
			derr = readBoxPayload(fh, bi, tb.stss)
		}
		if derr != nil {
			f.AddIssue(fault.From(derr, fault.StageIndex, fault.KindCorruptSample).
				At(tb.trackID(), int64(bi.Offset)))
		}
	}
	return nil
}

// checkMfra validates the trailing index when present.
func (f *File) checkMfra(fh *os.File) *fault.Error {
	if f.Mfra.Size == 0 {
		return nil
	}
	kids, err := childSpans(fh, f.Mfra.Offset+8, f.Mfra.End(), f.Limits.MaxBoxSize)
	if err != nil {
		return fault.From(err, fault.StageIndex, fault.KindMissingIndex).
			At(fault.UnknownTrack, int64(f.Mfra.Offset))
	}
	for _, bi := range kids {
		if bi.Type != gomp4.BoxTypeMfro() {
			continue
		}
		mfro := &gomp4.Mfro{}
		if err := readBoxPayload(fh, bi, mfro); err != nil {
			return fault.From(err, fault.StageIndex, fault.KindMissingIndex).
				At(fault.UnknownTrack, int64(f.Mfra.Offset))
		}
		if uint64(mfro.Size) != f.Mfra.Size {
			return fault.New(fault.StageIndex, fault.KindMissingIndex,
				"mfra size %d disagrees with mfro %d", f.Mfra.Size, mfro.Size).
				At(fault.UnknownTrack, int64(f.Mfra.Offset))
		}
		return nil
	}
	return fault.New(fault.StageIndex, fault.KindMissingIndex,
		"mfra at %d has no mfro", f.Mfra.Offset).At(fault.UnknownTrack, int64(f.Mfra.Offset))
}

// readPayload copies one sample payload into buf, never exceeding maxSize.
// The returned slice may alias buf; callers that keep it must copy.
func (f *File) readPayload(fh *os.File, s model.Sample, buf []byte, maxSize int) ([]byte, error) {
	if s.Size < 0 || s.Size > maxSize {
		return nil, fault.New(fault.StageParse, fault.KindInvalidLength,
			"sample %d size %d outside buffer limit %d", s.Number, s.Size, maxSize).
			At(s.Number, s.Offset)
	}
	if uint64(s.Offset)+uint64(s.Size) > uint64(f.Size) {
		return nil, fault.New(fault.StageParse, fault.KindTruncatedBox,
			"sample %d extends past end of file", s.Number).At(s.Number, s.Offset)
	}
	if cap(buf) < s.Size {
		buf = make([]byte, s.Size)
	}
	buf = buf[:s.Size]
	if _, err := fh.ReadAt(buf, s.Offset); err != nil {
		return nil, fault.From(err, fault.StageParse, fault.KindTruncatedBox).At(s.Number, s.Offset)
	}
	if f.marker && !hasSampleMarker(buf) {
		return nil, fault.New(fault.StageParse, fault.KindCorruptSample,
			"sample %d payload is missing its integrity marker", s.Number).At(s.Number, s.Offset)
	}
	return buf, nil
}

// sampleMarker is duplicated from testkit to avoid a production->fixture
// dependency; the generator writes the same four bytes.
var sampleMarker = [4]byte{'M', 'L', 'N', '1'}

func hasSampleMarker(buf []byte) bool {
	return len(buf) >= len(sampleMarker) && string(buf[:len(sampleMarker)]) == string(sampleMarker[:])
}

// SampleReader yields sample payloads from one source file with a fixed
// buffer. One reader belongs to one goroutine.
type SampleReader struct {
	file   *File
	fh     *os.File
	buf    []byte
	maxSz  int
	closed bool
}

// OpenSampleReader opens the source for sequential sample reads.
func (f *File) OpenSampleReader(maxSample int) (*SampleReader, error) {
	if maxSample <= 0 {
		maxSample = 1 << 20
	}
	fh, err := os.Open(f.Path)
	if err != nil {
		return nil, fault.From(err, fault.StageParse, fault.KindMissingIndex)
	}
	return &SampleReader{file: f, fh: fh, maxSz: maxSample}, nil
}

// ReadSample returns the payload bytes for s. The slice is reused across calls.
func (r *SampleReader) ReadSample(s model.Sample) ([]byte, error) {
	if r.closed {
		return nil, fault.New(fault.StageParse, fault.KindCorruptSample, "sample reader closed")
	}
	return r.file.readPayload(r.fh, s, r.buf, r.maxSz)
}

// Close releases the file handle.
func (r *SampleReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return r.fh.Close()
}

// String summarises the file for diagnostics.
func (f *File) String() string {
	return fmt.Sprintf("%s tracks=%d issues=%d mdat=[%d,%d)", f.Path, len(f.Tracks), len(f.Issues), f.Mdat.Offset, f.Mdat.End())
}
