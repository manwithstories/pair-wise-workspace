package mp4

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	gomp4 "github.com/abema/go-mp4"

	"medialine/internal/clock"
	"medialine/internal/fault"
	"medialine/internal/model"
)

// SegmentPlan is one output fragment handed to the writer. Samples are written
// in plan order; the writer decides payload placement so that chunk offsets are
// always internally consistent.
type SegmentPlan struct {
	ID          string
	TrackID     int
	Kind        model.SegmentKind
	Timescale   uint32
	Codec       string
	StartUS     int64
	EndUS       int64
	Samples     []model.Sample
	Payloads    [][]byte
	SourceTrack []int
}

// WriterOptions bounds the writer's behaviour.
type WriterOptions struct {
	// MaxPayload is the largest sample the writer accepts, in bytes.
	MaxPayload int
	// Root is the directory that relative segment paths must stay inside.
	Root string
}

// WriteSegment writes one deterministic MP4 containing only the planned
// samples. Layout is ftyp, mdat, moov, with samples packed contiguously in
// plan order, so the same plan always produces byte-identical output.
func WriteSegment(path string, plan SegmentPlan, opts WriterOptions) (int64, error) {
	if err := checkOutputPath(opts.Root, path); err != nil {
		return 0, err
	}
	if len(plan.Samples) == 0 {
		return 0, fault.New(fault.StageWrite, fault.KindBadConfig, "segment %s has no samples", plan.ID).
			WithSegment(plan.ID)
	}
	if plan.Timescale == 0 {
		return 0, fault.New(fault.StageWrite, fault.KindBadConfig, "segment %s has zero timescale", plan.ID).
			WithSegment(plan.ID)
	}
	maxPayload := opts.MaxPayload
	if maxPayload <= 0 {
		maxPayload = DefaultMaxBoxSize
	}
	total := 0
	for i, s := range plan.Samples {
		if s.Size < 0 || s.Size > maxPayload {
			return 0, fault.New(fault.StageWrite, fault.KindInvalidLength,
				"sample %d size %d exceeds limit %d", s.Number, s.Size, maxPayload).
				WithSegment(plan.ID).At(plan.TrackID, s.Offset)
		}
		if i >= len(plan.Payloads) || len(plan.Payloads[i]) != s.Size {
			return 0, fault.New(fault.StageWrite, fault.KindCorruptSample,
				"sample %d payload does not match declared size %d", s.Number, s.Size).
				WithSegment(plan.ID).At(plan.TrackID, s.Offset)
		}
		total += s.Size
	}

	ftyp, err := buildFtyp()
	if err != nil {
		return 0, fault.From(err, fault.StageWrite, fault.KindWrite).WithSegment(plan.ID)
	}
	const mdatHeader = 8
	dataStart := len(ftyp) + mdatHeader
	chunkOffsets := make([]uint32, len(plan.Samples))
	off := uint64(dataStart)
	for i, s := range plan.Samples {
		if off > math.MaxUint32 {
			return 0, fault.New(fault.StageWrite, fault.KindInvalidLength,
				"segment offset %d exceeds 32-bit stco", off).WithSegment(plan.ID)
		}
		chunkOffsets[i] = uint32(off)
		off += uint64(s.Size)
	}
	moov, err := buildMoov(plan, chunkOffsets)
	if err != nil {
		return 0, fault.From(err, fault.StageWrite, fault.KindWrite).WithSegment(plan.ID)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, fault.From(err, fault.StageWrite, fault.KindPathEscape).WithSegment(plan.ID)
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, fault.From(err, fault.StageWrite, fault.KindWrite).WithSegment(plan.ID)
	}
	abort := func(err error) (int64, error) {
		f.Close()
		os.Remove(tmp)
		return 0, fault.From(err, fault.StageWrite, fault.KindWrite).WithSegment(plan.ID)
	}
	if _, err := f.Write(ftyp); err != nil {
		return abort(err)
	}
	hdr := []byte{0, 0, 0, 0, 'm', 'd', 'a', 't'}
	n := uint32(mdatHeader + total)
	hdr[0], hdr[1], hdr[2], hdr[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	if _, err := f.Write(hdr); err != nil {
		return abort(err)
	}
	for i := range plan.Payloads {
		if _, err := f.Write(plan.Payloads[i]); err != nil {
			return abort(err)
		}
	}
	if _, err := f.Write(moov); err != nil {
		return abort(err)
	}
	if err := f.Sync(); err != nil {
		return abort(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return 0, fault.From(err, fault.StageWrite, fault.KindWrite).WithSegment(plan.ID)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return 0, fault.From(err, fault.StageWrite, fault.KindWrite).WithSegment(plan.ID)
	}
	st, err := os.Stat(path)
	if err != nil {
		return 0, fault.From(err, fault.StageWrite, fault.KindWrite).WithSegment(plan.ID)
	}
	return st.Size(), nil
}

// checkOutputPath refuses NUL bytes and paths escaping the output root.
func checkOutputPath(root, path string) error {
	if strings.ContainsRune(path, 0) {
		return fault.New(fault.StageWrite, fault.KindPathEscape, "output path contains NUL")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fault.From(err, fault.StageWrite, fault.KindPathEscape)
	}
	if root == "" {
		return nil
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return fault.From(err, fault.StageWrite, fault.KindPathEscape)
	}
	rel, err := filepath.Rel(rootAbs, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fault.New(fault.StageWrite, fault.KindPathEscape,
			"output %s escapes root %s", abs, rootAbs)
	}
	return nil
}

func buildFtyp() ([]byte, error) {
	ftyp := &gomp4.Ftyp{MajorBrand: gomp4.BrandISOM()}
	ftyp.AddCompatibleBrand(gomp4.BrandISOM())
	ftyp.AddCompatibleBrand(gomp4.BrandISO6())
	payload := &sliceSeeker{}
	if _, err := gomp4.Marshal(payload, ftyp, gomp4.Context{}); err != nil {
		return nil, err
	}
	body := payload.Bytes()
	out := make([]byte, 8+len(body))
	out[0], out[1], out[2], out[3] = byte(len(out)>>24), byte(len(out)>>16), byte(len(out)>>8), byte(len(out))
	copy(out[4:8], "ftyp")
	copy(out[8:], body)
	return out, nil
}

// buildMoov emits a one-track moov describing the planned samples with one
// sample per chunk, so chunk offsets are explicit.
func buildMoov(plan SegmentPlan, chunkOffsets []uint32) ([]byte, error) {
	spanUS := plan.EndUS - plan.StartUS
	ticks, err := clock.MicrosToTicks(spanUS, uint64(plan.Timescale))
	if err != nil {
		return nil, err
	}
	tkhd := &gomp4.Tkhd{TrackID: uint32(plan.TrackID)}
	tkhd.SetFlags(0x3)
	if plan.Codec == "soun" {
		tkhd.Volume = 0x0100
	}
	tkhd.Matrix = identityMatrixValues()
	if ticks <= math.MaxUint32 {
		tkhd.DurationV0 = uint32(ticks)
	} else {
		tkhd.SetVersion(1)
		tkhd.DurationV1 = uint64(ticks)
	}

	mdhd := &gomp4.Mdhd{Timescale: plan.Timescale, Language: [3]byte{'u', 'n', 'd'}}
	if ticks <= math.MaxUint32 {
		mdhd.DurationV0 = uint32(ticks)
	} else {
		mdhd.SetVersion(1)
		mdhd.DurationV1 = uint64(ticks)
	}

	handler := "vide"
	if plan.Codec == "soun" {
		handler = "soun"
	}
	hdlr := &gomp4.Hdlr{HandlerType: [4]byte{handler[0], handler[1], handler[2], handler[3]}, Name: "medialine\x00"}

	stts := &gomp4.Stts{}
	var sampleTicks []uint32
	for _, s := range plan.Samples {
		t, err := clock.MicrosToTicks(s.DurationUS, uint64(plan.Timescale))
		if err != nil {
			return nil, err
		}
		if t < 0 || t > math.MaxUint32 {
			return nil, fmt.Errorf("sample duration %dus out of range at scale %d", s.DurationUS, plan.Timescale)
		}
		sampleTicks = append(sampleTicks, uint32(t))
		stts.Entries = appendStts(stts.Entries, uint32(t))
	}
	stts.EntryCount = uint32(len(stts.Entries))

	stsc := &gomp4.Stsc{EntryCount: 1, Entries: []gomp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: 1, SampleDescriptionIndex: 1}}}
	stsz := &gomp4.Stsz{SampleCount: uint32(len(plan.Samples))}
	stco := &gomp4.Stco{EntryCount: uint32(len(chunkOffsets)), ChunkOffset: chunkOffsets}
	allKey := true
	var syncSamples []uint32
	for i, s := range plan.Samples {
		stsz.EntrySize = append(stsz.EntrySize, uint32(s.Size))
		if s.Keyframe {
			syncSamples = append(syncSamples, uint32(i+1))
		} else {
			allKey = false
		}
	}
	var stss *gomp4.Stss
	if !allKey {
		stss = &gomp4.Stss{EntryCount: uint32(len(syncSamples)), SampleNumber: syncSamples}
	}

	mvhd := &gomp4.Mvhd{Timescale: 1000, NextTrackID: uint32(plan.TrackID + 1), Rate: 0x00010000, Volume: 0x0100}
	mvhd.Matrix = identityMatrixValues()
	mvhd.DurationV0 = uint32(spanUS / 1000)

	out := &sliceSeeker{}
	w := gomp4.NewWriter(out)
	if _, err := w.StartBox(&gomp4.BoxInfo{Type: gomp4.BoxTypeMoov()}); err != nil {
		return nil, err
	}
	if err := writeBox(w, gomp4.BoxTypeMvhd(), mvhd); err != nil {
		return nil, err
	}
	if _, err := w.StartBox(&gomp4.BoxInfo{Type: gomp4.BoxTypeTrak()}); err != nil {
		return nil, err
	}
	if err := writeBox(w, gomp4.BoxTypeTkhd(), tkhd); err != nil {
		return nil, err
	}
	if _, err := w.StartBox(&gomp4.BoxInfo{Type: gomp4.BoxTypeMdia()}); err != nil {
		return nil, err
	}
	if err := writeBox(w, gomp4.BoxTypeMdhd(), mdhd); err != nil {
		return nil, err
	}
	if err := writeBox(w, gomp4.BoxTypeHdlr(), hdlr); err != nil {
		return nil, err
	}
	if _, err := w.StartBox(&gomp4.BoxInfo{Type: gomp4.BoxTypeMinf()}); err != nil {
		return nil, err
	}
	if plan.Codec == "soun" {
		if err := writeBox(w, gomp4.BoxTypeSmhd(), &gomp4.Smhd{}); err != nil {
			return nil, err
		}
	} else if err := writeBox(w, gomp4.BoxTypeVmhd(), &gomp4.Vmhd{}); err != nil {
		return nil, err
	}
	if _, err := w.StartBox(&gomp4.BoxInfo{Type: gomp4.BoxTypeStbl()}); err != nil {
		return nil, err
	}
	// stsd carries no sample entry: medialine fragments are table-only, and
	// the verifier intentionally does not decode media.
	if err := writeBox(w, gomp4.BoxTypeStsd(), &gomp4.Stsd{EntryCount: 0}); err != nil {
		return nil, err
	}
	for _, item := range []struct {
		typ gomp4.BoxType
		box gomp4.IBox
	}{
		{gomp4.BoxTypeStts(), stts},
		{gomp4.BoxTypeStsc(), stsc},
		{gomp4.BoxTypeStsz(), stsz},
		{gomp4.BoxTypeStco(), stco},
	} {
		if err := writeBox(w, item.typ, item.box); err != nil {
			return nil, err
		}
	}
	if stss != nil {
		if err := writeBox(w, gomp4.BoxTypeStss(), stss); err != nil {
			return nil, err
		}
	}
	for i := 0; i < 3; i++ { // stbl, minf, mdia
		if _, err := w.EndBox(); err != nil {
			return nil, err
		}
	}
	if _, err := w.EndBox(); err != nil { // trak
		return nil, err
	}
	if _, err := w.EndBox(); err != nil { // moov
		return nil, err
	}
	_ = sampleTicks
	return out.Bytes(), nil
}

func writeBox(w *gomp4.Writer, typ gomp4.BoxType, box gomp4.IBox) error {
	if _, err := w.StartBox(&gomp4.BoxInfo{Type: typ}); err != nil {
		return err
	}
	if _, err := gomp4.Marshal(w, box, gomp4.Context{}); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

func appendStts(entries []gomp4.SttsEntry, durTicks uint32) []gomp4.SttsEntry {
	if n := len(entries); n > 0 && entries[n-1].SampleDelta == durTicks {
		entries[n-1].SampleCount++
		return entries
	}
	return append(entries, gomp4.SttsEntry{SampleCount: 1, SampleDelta: durTicks})
}

func identityMatrixValues() [9]int32 {
	return [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
}

// sliceSeeker is an in-memory io.WriteSeeker used to marshal boxes before the
// final file layout is known.
type sliceSeeker struct {
	buf []byte
	pos int64
}

func (s *sliceSeeker) Write(p []byte) (int, error) {
	if end := s.pos + int64(len(p)); end > int64(len(s.buf)) {
		grown := make([]byte, end)
		copy(grown, s.buf)
		s.buf = grown
	}
	copy(s.buf[s.pos:], p)
	s.pos += int64(len(p))
	return len(p), nil
}

func (s *sliceSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0:
		s.pos = offset
	case 1:
		s.pos += offset
	case 2:
		s.pos = int64(len(s.buf)) + offset
	}
	if s.pos < 0 {
		return 0, fmt.Errorf("negative seek")
	}
	if s.pos > int64(len(s.buf)) {
		grown := make([]byte, s.pos)
		copy(grown, s.buf)
		s.buf = grown
	}
	return s.pos, nil
}

func (s *sliceSeeker) Bytes() []byte { return s.buf }
