// Package index writes the recovered media as a fragmented MP4
// (one fragment per keyframe group) so every fragment boundary is a
// random-access point, and inspects written files for verification.
package index

import (
	"fmt"
	"io"

	"github.com/Eyevinn/mp4ff/mp4"
)

// TrackMeta describes one output track.
type TrackMeta struct {
	TrackID   uint32
	SSRC      uint32
	Timescale uint32
	MediaType string // "video" or "audio"
}

// SampleIn is one media sample with its repaired decode time.
type SampleIn struct {
	TrackID    uint32
	Data       []byte
	DecodeTime uint64 // in track timescale units
	Dur        uint32
	Sync       bool
}

// PendingSample is the serializable form of a buffered sample.
type PendingSample struct {
	Data       []byte
	DecodeTime uint64
	Dur        uint32
	Sync       bool
}

// State is the serializable writer state for checkpointing.
type State struct {
	FragSeq uint32
	Pending map[uint32][]PendingSample
}

// Writer builds a fragmented MP4 incrementally. Fragments use
// default-base-is-moof, so no box contains an absolute file offset
// and appending after resume is safe.
type Writer struct {
	w       io.Writer
	at      int64
	metas   []TrackMeta
	fragSeq uint32
	pending map[uint32][]PendingSample
	inited  bool
}

func NewWriter(w io.Writer, metas []TrackMeta) *Writer {
	return &Writer{w: w, metas: metas, pending: map[uint32][]PendingSample{}}
}

// WriteInit writes ftyp+moov; must be called once before samples.
func (w *Writer) WriteInit() error {
	init := mp4.CreateEmptyInit()
	for _, m := range w.metas {
		init.AddEmptyTrack(m.Timescale, m.MediaType, "und")
	}
	cw := &countingWriter{w: w.w}
	if err := init.Encode(cw); err != nil {
		return err
	}
	w.at += cw.n
	w.inited = true
	return nil
}

// AddSample buffers a sample for the current fragment.
func (w *Writer) AddSample(s SampleIn) {
	w.pending[s.TrackID] = append(w.pending[s.TrackID], PendingSample{
		Data: s.Data, DecodeTime: s.DecodeTime, Dur: s.Dur, Sync: s.Sync,
	})
}

// PendingVideo reports whether any samples await a fragment boundary.
func (w *Writer) Pending() int {
	n := 0
	for _, p := range w.pending {
		n += len(p)
	}
	return n
}

// FlushFragment writes the buffered samples as one fragment and
// returns the committed byte range [start, end).
func (w *Writer) FlushFragment() (int64, int64, error) {
	if w.Pending() == 0 {
		return w.at, w.at, nil
	}
	w.fragSeq++
	trackIDs := make([]uint32, 0, len(w.metas))
	for _, m := range w.metas {
		trackIDs = append(trackIDs, m.TrackID)
	}
	frag, err := mp4.CreateMultiTrackFragment(w.fragSeq, trackIDs)
	if err != nil {
		return 0, 0, err
	}
	for _, m := range w.metas {
		for _, p := range w.pending[m.TrackID] {
			flags := uint32(mp4.NonSyncSampleFlags)
			if p.Sync {
				flags = mp4.SyncSampleFlags
			}
			fs := mp4.FullSample{
				Sample: mp4.Sample{Flags: flags, Dur: p.Dur, Size: uint32(len(p.Data))},
				DecodeTime: p.DecodeTime,
				Data:       p.Data,
			}
			if err := frag.AddFullSampleToTrack(fs, m.TrackID); err != nil {
				return 0, 0, fmt.Errorf("track %d: %w", m.TrackID, err)
			}
		}
	}
	start := w.at
	cw := &countingWriter{w: w.w}
	if err := frag.Encode(cw); err != nil {
		return 0, 0, err
	}
	w.at += cw.n
	w.pending = map[uint32][]PendingSample{}
	return start, w.at, nil
}

// Offset returns the number of bytes committed so far.
func (w *Writer) Offset() int64 { return w.at }

// FragSeq returns the next fragment sequence number minus one.
func (w *Writer) FragSeq() uint32 { return w.fragSeq }

// State exports pending samples for checkpointing.
func (w *Writer) State() *State {
	st := &State{FragSeq: w.fragSeq, Pending: map[uint32][]PendingSample{}}
	for k, v := range w.pending {
		st.Pending[k] = append([]PendingSample(nil), v...)
	}
	return st
}

// Restore loads a previously exported state.
func (w *Writer) Restore(st *State, committedOffset int64) {
	w.fragSeq = st.FragSeq
	w.pending = map[uint32][]PendingSample{}
	for k, v := range st.Pending {
		w.pending[k] = append([]PendingSample(nil), v...)
	}
	w.at = committedOffset
	w.inited = true
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// FragmentInfo summarizes one fragment for verification.
type FragmentInfo struct {
	Seq        uint32
	DecodeTime map[uint32]uint64 // trackID -> tfdt
	FirstSync  map[uint32]bool   // trackID -> first sample sync flag
	Samples    map[uint32]int
}

// Inspect parses a fragmented MP4 and returns per-fragment info.
func Inspect(r io.Reader) ([]FragmentInfo, error) {
	f, err := mp4.DecodeFile(r)
	if err != nil {
		return nil, err
	}
	if f.Moov == nil {
		return nil, fmt.Errorf("no moov box: not a valid MP4")
	}
	var out []FragmentInfo
	for _, seg := range f.Segments {
		for _, frag := range seg.Fragments {
			fi := FragmentInfo{
				Seq:        frag.Moof.Mfhd.SequenceNumber,
				DecodeTime: map[uint32]uint64{},
				FirstSync:  map[uint32]bool{},
				Samples:    map[uint32]int{},
			}
			for _, traf := range frag.Moof.Trafs {
				id := traf.Tfhd.TrackID
				if traf.Tfdt != nil {
					fi.DecodeTime[id] = traf.Tfdt.BaseMediaDecodeTime()
				}
				for _, trun := range traf.Truns {
					samples := trun.GetSamples()
					fi.Samples[id] += len(samples)
					if len(samples) > 0 {
						if _, seen := fi.FirstSync[id]; !seen {
							fi.FirstSync[id] = mp4.IsSyncSampleFlags(samples[0].Flags)
						}
					}
				}
			}
			out = append(out, fi)
		}
	}
	return out, nil
}
