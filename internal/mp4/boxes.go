// Package mp4 reads track timebases, sample tables and keyframe tables from
// local MP4/MOV files. It validates structural invariants and reports defects
// with the shared model error codes so callers can separate source corruption
// from transport loss.
package mp4

import (
	"fmt"
	"io"
	"os"

	gomp4 "github.com/abema/go-mp4"

	"archive.local/archive/internal/model"
)

// Track is the parsed sample table of one media track.
type Track struct {
	TrackID   uint32
	Timescale uint32
	Origin    int64 // source media time of the first sample, retained
	// ContainerTime is the media time declared by the container's edit list
	// (0 when absent). It is diagnostic only; the manifest origin drives the
	// unified timeline.
	ContainerTime int64
	Codec         string
	EditList      []EditRun
	Samples       []Sample
	Sync          []uint32 // 1-based sample numbers that are decodable entry points
	Defects       []*model.Error
	FileSize      int64
	// Complete is the number of leading samples whose payload lie wholly
	// inside the file. Samples at or beyond this index are unrecoverable.
	Complete int
}

// EditRun mirrors an edit-list run in media ticks. A negative media time is
// an empty edit (presentation gap).
type EditRun struct {
	MediaTime       int64
	SegmentDuration int64 // -1 means "rest of media"
}

// Sample is one row of the sample table, retaining both clocks.
type Sample struct {
	Index        int
	Size         uint32
	Offset       uint64
	Duration     uint32
	SourceTicks  int64
	UnifiedTicks int64
	Sync         bool
}

// ParseFile opens path and extracts the track declared by the manifest.
func ParseFile(path string, trackID uint32, tl *model.Timeline) (*Track, error) {
	f, err := os.Open(path)
	if err != nil {
		code := model.CodeSourceCorrupt
		detail := "cannot open source"
		if os.IsNotExist(err) {
			// A source named by the manifest but absent on disk is a broken
			// reference, not corruption of the media itself.
			code = model.CodeManifestReference
			detail = "manifest source file does not exist"
		}
		return nil, &model.Error{Code: code, File: path, Track: trackID,
			Sample: -1, OffsetBytes: -1, Detail: detail, Err: err}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, &model.Error{Code: model.CodeSourceCorrupt, File: path, Track: trackID,
			Sample: -1, OffsetBytes: -1, Detail: "cannot stat source", Err: err}
	}
	return Parse(f, st.Size(), path, trackID, tl)
}

// Parse reads box structure from r, which must span the whole file. On source
// damage it returns a partially filled Track together with the first defect.
func Parse(r io.ReadSeeker, fileSize int64, path string, trackID uint32, tl *model.Timeline) (*Track, error) {
	tb, ok := tl.Base(trackID)
	if !ok {
		return nil, &model.Error{Code: model.CodeManifestReference, File: path, Track: trackID,
			Sample: -1, OffsetBytes: -1, Detail: "track missing from the timeline"}
	}
	if fileSize <= 0 {
		return nil, &model.Error{Code: model.CodeSourceCorrupt, File: path, Track: trackID,
			Sample: -1, OffsetBytes: 0, Detail: "empty source file"}
	}
	trak, err := findTrak(r, path, trackID)
	if err != nil {
		return nil, err
	}
	tr := &Track{TrackID: trackID, Timescale: tb.Timescale, Origin: tb.OriginTicks, FileSize: fileSize}

	tkhd, elst, mdhd, stsd, stts, stsc, stsz, stss, stco, co64, err :=
		readChildren(r, trak, path, trackID)
	if err != nil {
		return nil, err
	}
	_ = tkhd
	if mdhd == nil {
		return nil, defect(path, trackID, model.CodeSourceCorrupt, -1, -1, "mdhd missing", nil)
	}
	if stts == nil || stsc == nil || stsz == nil {
		return nil, defect(path, trackID, model.CodeSourceCorrupt, -1, -1, "sample table boxes missing", nil)
	}
	if mdhd.Timescale != tb.Timescale {
		return nil, defect(path, trackID, model.CodeManifestReference, -1, -1,
			fmt.Sprintf("manifest timescale %d disagrees with mdhd %d", tb.Timescale, mdhd.Timescale), nil)
	}
	tr.Timescale = mdhd.Timescale
	if elst != nil {
		for i := range elst.Entries {
			tr.EditList = append(tr.EditList, EditRun{
				MediaTime:       elst.GetMediaTime(i),
				SegmentDuration: int64(elst.GetSegmentDuration(i)),
			})
		}
		// The edit's media time describes where the media sits inside the
		// container. The manifest's declared origin remains canonical for the
		// unified timeline; the container value is retained in EditList.
		tr.ContainerTime = elst.GetMediaTime(0)
	}
	tr.Codec = readCodec(r, stsd)

	if err := tr.buildSamples(stts, stsc, stsz, stco, co64, path, tl, tb); err != nil {
		return tr, err
	}
	tr.Sync, err = resolveSync(stss, tr, path, trackID)
	if err != nil {
		me, ok := err.(*model.Error)
		if !ok {
			return tr, defect(path, trackID, model.CodeSourceCorrupt, -1, -1, err.Error(), nil)
		}
		tr.Defects = append(tr.Defects, me)
		tr.Sync = []uint32{1}
		for i := range tr.Samples {
			tr.Samples[i].Sync = i == 0
		}
	} else {
		for i := range tr.Samples {
			tr.Samples[i].Sync = containsSorted(tr.Sync, uint32(i+1))
		}
	}
	// Payload extents must lie inside the file. A short tail is recoverable:
	// the leading Complete samples stay usable.
	if d := validateRanges(tr, path, trackID); d != nil {
		tr.Defects = append(tr.Defects, d)
		return tr, d
	}
	tr.Complete = len(tr.Samples)
	if len(tr.Defects) > 0 {
		return tr, tr.Defects[0]
	}
	return tr, nil
}

func defect(path string, trackID uint32, code model.Code, sample int, off int64, detail string, rec *model.Recoverable) *model.Error {
	return &model.Error{Code: code, File: path, Track: trackID, Sample: sample, OffsetBytes: off,
		Detail: detail, Recoverable: rec}
}

// findTrak locates the trak box holding the requested track ID.
func findTrak(r io.ReadSeeker, path string, trackID uint32) (*gomp4.BoxInfo, error) {
	traks, err := gomp4.ExtractBoxes(r, nil, []gomp4.BoxPath{
		{gomp4.BoxTypeMoov(), gomp4.BoxTypeTrak()},
	})
	if err != nil {
		return nil, defect(path, trackID, model.CodeSourceCorrupt, -1, -1, "malformed moov structure", nil)
	}
	for _, trak := range traks {
		tk, err := gomp4.ExtractBoxWithPayload(r, trak, gomp4.BoxPath{gomp4.BoxTypeTkhd()})
		if err != nil || len(tk) == 0 {
			continue
		}
		if h, ok := tk[0].Payload.(*gomp4.Tkhd); ok && h.TrackID == trackID {
			return trak, nil
		}
	}
	return nil, defect(path, trackID, model.CodeManifestReference, -1, -1,
		"manifest track not present in source", nil)
}

// readChildren extracts the sample table boxes owned by one trak.
func readChildren(r io.ReadSeeker, trak *gomp4.BoxInfo, path string, trackID uint32) (
	tkhd *gomp4.Tkhd, elst *gomp4.Elst, mdhd *gomp4.Mdhd, stsd *gomp4.Stsd,
	stts *gomp4.Stts, stsc *gomp4.Stsc, stsz *gomp4.Stsz, stss *gomp4.Stss,
	stco *gomp4.Stco, co64 *gomp4.Co64, err error) {
	bips, e := gomp4.ExtractBoxesWithPayload(r, trak, []gomp4.BoxPath{
		{gomp4.BoxTypeTkhd()},
		{gomp4.BoxTypeEdts(), gomp4.BoxTypeElst()},
		{gomp4.BoxTypeMdia(), gomp4.BoxTypeMdhd()},
		{gomp4.BoxTypeMdia(), gomp4.BoxTypeMinf(), gomp4.BoxTypeStbl(), gomp4.BoxTypeStsd()},
		{gomp4.BoxTypeMdia(), gomp4.BoxTypeMinf(), gomp4.BoxTypeStbl(), gomp4.BoxTypeStts()},
		{gomp4.BoxTypeMdia(), gomp4.BoxTypeMinf(), gomp4.BoxTypeStbl(), gomp4.BoxTypeStsc()},
		{gomp4.BoxTypeMdia(), gomp4.BoxTypeMinf(), gomp4.BoxTypeStbl(), gomp4.BoxTypeStsz()},
		{gomp4.BoxTypeMdia(), gomp4.BoxTypeMinf(), gomp4.BoxTypeStbl(), gomp4.BoxTypeStss()},
		{gomp4.BoxTypeMdia(), gomp4.BoxTypeMinf(), gomp4.BoxTypeStbl(), gomp4.BoxTypeStco()},
		{gomp4.BoxTypeMdia(), gomp4.BoxTypeMinf(), gomp4.BoxTypeStbl(), gomp4.BoxTypeCo64()},
	})
	_ = path
	_ = trackID
	if e != nil {
		err = e
		return
	}
	for _, b := range bips {
		switch p := b.Payload.(type) {
		case *gomp4.Tkhd:
			tkhd = p
		case *gomp4.Elst:
			elst = p
		case *gomp4.Mdhd:
			mdhd = p
		case *gomp4.Stsd:
			stsd = p
		case *gomp4.Stts:
			stts = p
		case *gomp4.Stsc:
			stsc = p
		case *gomp4.Stsz:
			stsz = p
		case *gomp4.Stss:
			stss = p
		case *gomp4.Stco:
			stco = p
		case *gomp4.Co64:
			co64 = p
		}
	}
	return
}

func readCodec(_ io.ReadSeeker, stsd *gomp4.Stsd) string {
	if stsd == nil || stsd.EntryCount == 0 {
		return "unknown"
	}
	return "sample-entry"
}

func (t *Track) buildSamples(stts *gomp4.Stts, stsc *gomp4.Stsc, stsz *gomp4.Stsz,
	stco *gomp4.Stco, co64 *gomp4.Co64, path string, tl *model.Timeline, tb model.Timebase) error {
	total := 0
	for _, e := range stts.Entries {
		if e.SampleCount > 0 && e.SampleDelta > 0 && uint64(e.SampleCount) > (1<<62)/uint64(e.SampleDelta) {
			return defect(path, t.TrackID, model.CodeTimescaleOverflow, len(t.Samples), -1,
				"sample duration table overflows the media clock", nil)
		}
		total += int(e.SampleCount)
	}
	sizes, err := expandStsz(stsz, path, t.TrackID)
	if err != nil {
		return err
	}
	if total != len(sizes) {
		return defect(path, t.TrackID, model.CodeTruncatedSample, minInt(total, len(sizes)), -1,
			fmt.Sprintf("stts declares %d samples but stsz declares %d", total, len(sizes)), nil)
	}
	offs, err := expandChunkOffsets(stsc, stco, co64, sizes, path, t.TrackID)
	if err != nil {
		return err
	}
	t.Samples = make([]Sample, len(sizes))
	src := t.Origin
	for i := range t.Samples {
		t.Samples[i] = Sample{Index: i, Size: sizes[i], Offset: offs[i], SourceTicks: src}
		u, err := tl.Unified(t.TrackID, src)
		if err != nil {
			return fmt.Errorf("track %d sample %d: %w", t.TrackID, i, err)
		}
		t.Samples[i].UnifiedTicks = u
		delta := uint32(0)
		if i < len(sizes) && i < total {
			delta = sttsDelta(stts, i)
		}
		t.Samples[i].Duration = delta
		src += int64(delta)
	}
	return nil
}
