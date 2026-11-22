// Package parse turns ISO BMFF containers and RTP streams into unified
// nanosecond sample events for the scan and timeline subsystems.
package parse

import (
	"fmt"
	"os"

	mp4 "github.com/abema/go-mp4"

	"avscan/internal/model"
)

// MP4Result is the outcome of one container parse.
type MP4Result struct {
	Timeline *model.Timeline
	// GlobalFaults are not tied to a specific track (e.g. truncated moov).
	GlobalFaults []model.Fault
	FileSize     int64
}

// mp4State accumulates box data while the go-mp4 walker descends the tree.
type mp4State struct {
	source   string
	fileSize int64
	tracks   []*model.Track
	tables   []*tableState // aligned 1:1 with tracks
	cur      *model.Track  // track currently being descended
	movieTS  uint32
	faults   []model.Fault
}

// curTable returns the sample-table accumulator of the current track.
func (st *mp4State) curTable() *tableState {
	if len(st.tables) == 0 {
		return nil
	}
	return st.tables[len(st.tables)-1]
}

// ParseMP4 reads a self-generated container and projects every sample onto
// the nanosecond time base. Structural defects are returned as faults rather
// than aborting the whole walk where recovery is possible.
func ParseMP4(path string) (*MP4Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st := &mp4State{source: path, fileSize: info.Size()}
	handler := func(h *mp4.ReadHandle) (interface{}, error) {
		st.handleBox(h)
		return h.Expand()
	}
	if _, err := mp4.ReadBoxStructure(f, handler); err != nil {
		// A truncated file ends here; record the byte position if the walker
		// supplies one, then keep whatever tracks were fully descended.
		st.faults = append(st.faults, model.NewByteFault(
			model.FaultTruncatedBox, path, st.fileSize,
			"box structure walk ended: "+err.Error()))
	}
	// Enforce structural limits before allocating per-sample event slices.
	if len(st.tracks) > model.MaxTracks {
		return nil, fmt.Errorf("%w: %d tracks > %d",
			model.ErrLimitExceeded, len(st.tracks), model.MaxTracks)
	}
	var totalSamples uint64
	for _, tb := range st.tables {
		if n, ok := tb.sampleCount(); ok {
			totalSamples += uint64(n)
		}
	}
	if totalSamples > model.MaxSamples {
		return nil, fmt.Errorf("%w: %d samples > %d",
			model.ErrLimitExceeded, totalSamples, model.MaxSamples)
	}
	// Project each track's tables onto the nanosecond time base.
	for i, tr := range st.tracks {
		events, tFaults := st.tables[i].buildEvents(tr, st.source, st.fileSize)
		tr.Samples = events
		tr.SyncSamples = st.tables[i].syncSamples()
		tr.ParseFaults = append(tr.ParseFaults, tFaults...)
	}
	tl := &model.Timeline{MovieTimescale: st.movieTS, Tracks: st.tracks}
	return &MP4Result{Timeline: tl, GlobalFaults: st.faults, FileSize: st.fileSize}, nil
}

// handleBox dispatches one box visit. Payload reads are selective so a single
// bad box never prevents the remaining tracks from being examined.
func (st *mp4State) handleBox(h *mp4.ReadHandle) {
	t := h.Path[len(h.Path)-1]
	switch t {
	case mp4.BoxTypeMvhd():
		st.readMvhd(h)
	case mp4.BoxTypeTrak():
		st.cur = &model.Track{}
		st.tracks = append(st.tracks, st.cur)
		st.tables = append(st.tables, newTableState())
	case mp4.BoxTypeTkhd():
		st.readTkhd(h)
	case mp4.BoxTypeMdhd():
		st.readMdhd(h)
	case mp4.BoxTypeHdlr():
		st.readHdlr(h)
	case mp4.BoxTypeElst():
		st.readElst(h)
	case mp4.BoxTypeStts(), mp4.BoxTypeStsc(), mp4.BoxTypeStsz(),
		mp4.BoxTypeStss(), mp4.BoxTypeCo64(), mp4.BoxTypeStco():
		if tb := st.curTable(); tb != nil {
			tb.readTable(h, st.source)
		}
	case mp4.BoxTypeColr():
		st.readColr(h)
	}
	// Sample-entry metadata lives one level under stsd.
	if len(h.Path) >= 2 && h.Path[len(h.Path)-2] == mp4.BoxTypeStsd() {
		st.readSampleEntry(h, t)
	}
}

func (st *mp4State) readMvhd(h *mp4.ReadHandle) {
	box, _, err := h.ReadPayload()
	if err != nil {
		st.faults = append(st.faults, byteFault(h, st.source, "mvhd payload: "+err.Error()))
		return
	}
	if mvhd, ok := box.(*mp4.Mvhd); ok {
		st.movieTS = mvhd.Timescale
	}
}

func (st *mp4State) readTkhd(h *mp4.ReadHandle) {
	box, _, err := h.ReadPayload()
	if err != nil || st.cur == nil {
		return
	}
	if tkhd, ok := box.(*mp4.Tkhd); ok {
		st.cur.ID = tkhd.TrackID
	}
}

func (st *mp4State) readMdhd(h *mp4.ReadHandle) {
	box, _, err := h.ReadPayload()
	if err != nil || st.cur == nil {
		return
	}
	if mdhd, ok := box.(*mp4.Mdhd); ok {
		st.cur.Timescale = mdhd.Timescale
		st.cur.Duration = mdhd.GetDuration()
	}
}

func (st *mp4State) readHdlr(h *mp4.ReadHandle) {
	box, _, err := h.ReadPayload()
	if err != nil || st.cur == nil {
		return
	}
	hdlr, ok := box.(*mp4.Hdlr)
	if !ok {
		return
	}
	switch string(hdlr.HandlerType[:]) {
	case "vide":
		st.cur.Kind = model.KindVideo
	case "soun":
		st.cur.Kind = model.KindAudio
	case "meta":
		st.cur.Kind = model.KindMarkers
	default:
		st.cur.Kind = model.KindUnknown
	}
}

func byteFault(h *mp4.ReadHandle, source, msg string) model.Fault {
	return model.NewByteFault(model.FaultTruncatedBox, source,
		int64(h.BoxInfo.Offset), msg)
}

// guardOffset reports integer boundary violations before arithmetic.
func guardOffset(off, size uint64, fileSize int64) bool {
	if off > uint64(fileSize) {
		return false
	}
	if size > uint64(fileSize)-off {
		return false
	}
	return true
}
