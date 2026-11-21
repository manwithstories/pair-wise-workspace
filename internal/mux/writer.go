// Package mux writes a target MP4 that carries every compressed sample
// verbatim. The byte layout is fully derived from the input index before the
// first byte is written, so interrupted and uninterrupted runs produce
// identical output and stage boundaries are known in advance.
package mux

import (
	"encoding/binary"
	"io"
	"os"

	"mediamerge/internal/demux"
	"mediamerge/internal/mapping"
	"mediamerge/internal/model"
	"mediamerge/internal/mp4util"
)

// TrackLayout describes the deterministic byte layout of one output track.
type TrackLayout struct {
	Track   *demux.Track
	Start   int64 // absolute offset of first sample byte
	Bytes   int64
	Samples int
}

// Layout holds all precomputed offsets.
type Layout struct {
	HeaderLen  int64 // bytes of ftyp + mdat header
	PayloadLen int64
	MovieEnd   int64 // offset after mdat, where moov begins
	Tracks     []TrackLayout
}

type layoutState struct {
	ftyp       []byte
	mdatHeader []byte
	positions  [][]int64
	plan       *mapping.Plan
	layout     *Layout
}

func computeLayout(plan *mapping.Plan) *layoutState {
	ftyp := mp4util.Box("ftyp", mp4util.Cat(
		mp4util.FourCC("isom"), mp4util.U32(0x200),
		mp4util.FourCC("isom"), mp4util.FourCC("iso2"),
		mp4util.FourCC("mp4a"), mp4util.FourCC("mp41"),
	))
	lay := &Layout{HeaderLen: int64(len(ftyp))}
	ls := &layoutState{
		ftyp: ftyp, plan: plan, layout: lay,
		positions: make([][]int64, len(plan.Tracks)),
	}
	pos := lay.HeaderLen + 16
	for i, t := range plan.Tracks {
		tl := TrackLayout{Track: t, Start: pos, Samples: len(t.Samples)}
		ls.positions[i] = make([]int64, len(t.Samples))
		p := pos
		for si, s := range t.Samples {
			ls.positions[i][si] = p
			p += int64(s.Size)
		}
		tl.Bytes = p - pos
		lay.PayloadLen += tl.Bytes
		pos = p
		lay.Tracks = append(lay.Tracks, tl)
	}
	lay.MovieEnd = lay.HeaderLen + 16 + lay.PayloadLen
	ls.mdatHeader = make([]byte, 16)
	binary.BigEndian.PutUint32(ls.mdatHeader[0:4], 1)
	copy(ls.mdatHeader[4:8], "mdat")
	binary.BigEndian.PutUint64(ls.mdatHeader[8:16], uint64(16+lay.PayloadLen))
	return ls
}

// PlanLayout exposes the computed layout without touching the filesystem.
func PlanLayout(plan *mapping.Plan) *Layout { return computeLayout(plan).layout }

// Writer appends samples to an output file.
type Writer struct {
	path       string
	file       *os.File
	plan       *mapping.Plan
	positions  [][]int64
	written    int64
	mdatEnd    int64
	nextSample []int
}

// New creates (or replaces) path and writes the ftyp and mdat header. Its
// return value is the stage-0 committed length.
func New(path string, plan *mapping.Plan) (*Writer, *Layout, error) {
	ls := computeLayout(plan)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, model.WrapError(model.StageMux, model.CodeWrite, 0, 0, false, err, "create output")
	}
	if _, err := f.Write(append(append([]byte(nil), ls.ftyp...), ls.mdatHeader...)); err != nil {
		f.Close()
		return nil, nil, model.WrapError(model.StageMux, model.CodeWrite, 0, 0, false, err, "write header")
	}
	w := &Writer{
		path: path, file: f, plan: plan, positions: ls.positions,
		written: ls.layout.HeaderLen + 16, mdatEnd: ls.layout.MovieEnd,
		nextSample: make([]int, len(plan.Tracks)),
	}
	return w, ls.layout, nil
}

// OpenAt opens an existing output and positions it at an arbitrary committed
// byte boundary, truncating any uncommitted tail. It returns the bytes rolled
// back. The boundary must fall exactly on a sample boundary or on the header.
func OpenAt(path string, plan *mapping.Plan, committed int64) (*Writer, *Layout, int64, error) {
	ls := computeLayout(plan)
	lay := ls.layout
	if committed < lay.HeaderLen+16 || committed > lay.MovieEnd {
		return nil, nil, 0, model.NewError(model.StageMux, model.CodeIntegrity, committed, 0, false,
			"committed boundary %d outside writable payload [%d,%d]",
			committed, lay.HeaderLen+16, lay.MovieEnd)
	}
	trackIdx := len(plan.Tracks)
	for i := range plan.Tracks {
		start := lay.Tracks[i].Start
		end := start + lay.Tracks[i].Bytes
		if committed < end {
			trackIdx = i
			break
		}
	}
	sampleIdx := 0
	if trackIdx < len(plan.Tracks) {
		for si, p := range ls.positions[trackIdx] {
			if p >= committed {
				break
			}
			sampleIdx = si + 1
		}
		if sampleIdx < len(ls.positions[trackIdx]) && ls.positions[trackIdx][sampleIdx] != committed {
			return nil, nil, 0, model.NewError(model.StageMux, model.CodeIntegrity, committed,
				plan.Tracks[trackIdx].TargetID, false, "boundary %d splits sample %d of track %d",
				committed, sampleIdx, plan.Tracks[trackIdx].TargetID)
		}
		if sampleIdx == len(ls.positions[trackIdx]) {
			if lay.Tracks[trackIdx].Start+lay.Tracks[trackIdx].Bytes != committed {
				return nil, nil, 0, model.NewError(model.StageMux, model.CodeIntegrity, committed,
					plan.Tracks[trackIdx].TargetID, false, "boundary %d is not a track edge", committed)
			}
			trackIdx++
			sampleIdx = 0
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return nil, nil, 0, model.WrapError(model.StageMux, model.CodeWrite, 0, 0, false, err,
			"open output for resume")
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, 0, model.WrapError(model.StageMux, model.CodeWrite, 0, 0, false, err, "stat output")
	}
	if info.Size() < committed {
		f.Close()
		return nil, nil, 0, model.NewError(model.StageMux, model.CodeIntegrity, info.Size(), 0, false,
			"output is %d bytes but committed boundary needs %d", info.Size(), committed)
	}
	rolledBack := info.Size() - committed
	if rolledBack > 0 {
		if err := f.Truncate(committed); err != nil {
			f.Close()
			return nil, nil, 0, model.WrapError(model.StageMux, model.CodeWrite, committed, 0, false, err,
				"truncate uncommitted tail")
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, nil, 0, model.WrapError(model.StageMux, model.CodeWrite, committed, 0, false, err,
				"sync rollback")
		}
	}
	if _, err := f.Seek(committed, io.SeekStart); err != nil {
		f.Close()
		return nil, nil, 0, model.WrapError(model.StageMux, model.CodeWrite, committed, 0, false, err,
			"seek resume point")
	}
	w := &Writer{
		path: path, file: f, plan: plan, positions: ls.positions,
		written: committed, mdatEnd: lay.MovieEnd,
		nextSample: make([]int, len(plan.Tracks)),
	}
	for i := 0; i < trackIdx && i < len(plan.Tracks); i++ {
		w.nextSample[i] = len(plan.Tracks[i].Samples)
	}
	if trackIdx < len(plan.Tracks) {
		w.nextSample[trackIdx] = sampleIdx
	}
	return w, lay, rolledBack, nil
}

// WriteSample appends one sample at its predetermined position.
func (w *Writer) WriteSample(trackIdx int, s model.Sample) error {
	if trackIdx < 0 || trackIdx >= len(w.positions) {
		return model.NewError(model.StageMux, model.CodeMapping, w.written, s.TrackID, false,
			"track index %d out of range", trackIdx)
	}
	si := w.nextSample[trackIdx]
	tr := w.plan.Tracks[trackIdx]
	if si >= len(tr.Samples) {
		return model.NewError(model.StageMux, model.CodeWrite, w.written, tr.TargetID, false,
			"track %d received extra sample %d", tr.TargetID, s.Index)
	}
	want := w.positions[trackIdx][si]
	if w.written != want {
		return model.NewError(model.StageMux, model.CodeWrite, w.written, tr.TargetID, false,
			"output position %d, expected %d for track %d sample %d", w.written, want, tr.TargetID, si)
	}
	if int64(len(s.Data)) != int64(tr.Samples[si].Size) {
		return model.NewError(model.StageMux, model.CodeWrite, w.written, tr.TargetID, false,
			"sample %d payload is %d bytes, expected %d", si, len(s.Data), tr.Samples[si].Size)
	}
	if _, err := w.file.Write(s.Data); err != nil {
		return model.WrapError(model.StageMux, model.CodeWrite, w.written, tr.TargetID, true, err,
			"write sample %d", si)
	}
	w.written += int64(len(s.Data))
	w.nextSample[trackIdx]++
	return nil
}

// WritePartial simulates a torn write for crash injection. It writes at most
// n bytes of sample payload and does not advance the writer state.
func (w *Writer) WritePartial(data []byte) (int, error) {
	return w.file.Write(data)
}

// Sync flushes written bytes to stable storage.
func (w *Writer) Sync() error {
	if w == nil || w.file == nil {
		return nil
	}
	if err := w.file.Sync(); err != nil {
		return model.WrapError(model.StageMux, model.CodeWrite, w.written, 0, true, err, "sync output")
	}
	return nil
}

// Finalize writes moov, fsyncs and closes. It returns the complete output
// length.
func (w *Writer) Finalize() (int64, error) {
	moov, err := w.buildMoov()
	if err != nil {
		return w.written, err
	}
	if _, err := w.file.Write(moov); err != nil {
		return w.written, model.WrapError(model.StageMux, model.CodeWrite, w.written, 0, false, err, "write moov")
	}
	w.written += int64(len(moov))
	if err := w.file.Sync(); err != nil {
		return w.written, model.WrapError(model.StageMux, model.CodeWrite, w.written, 0, false, err, "sync output")
	}
	if err := w.file.Close(); err != nil {
		w.file = nil
		return w.written, model.WrapError(model.StageMux, model.CodeWrite, w.written, 0, false, err, "close output")
	}
	w.file = nil
	return w.written, nil
}

// Close releases the output handle without finalizing.
func (w *Writer) Close() error {
	if w == nil || w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// Size returns bytes written so far.
func (w *Writer) Size() int64 { return w.written }

// NextSample returns the next sample index that will be written for a track.
func (w *Writer) NextSample(trackIdx int) int {
	if trackIdx < 0 || trackIdx >= len(w.nextSample) {
		return 0
	}
	return w.nextSample[trackIdx]
}
