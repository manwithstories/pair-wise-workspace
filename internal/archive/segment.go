// Package archive writes committed timeline events as fixed-duration
// segment files with atomic rename, per-segment SHA-256 and a JSON
// manifest, so identical input, parameters and seed always produce
// identical hashes.
package archive

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"archiver/internal/media"
	"archiver/internal/timeline"
)

// Params are the processing parameters recorded in the manifest and
// checkpoint; resume refuses to continue when they change.
type Params struct {
	Case       string   `json:"case"`
	Seed       uint64   `json:"seed"`
	Inject     []string `json:"inject"`
	LatenessUs int64    `json:"lateness_us"`
	HorizonUs  int64    `json:"horizon_us"`
	SliceUs    int64    `json:"slice_us"`
}

// Hash returns the canonical digest of the parameters.
func (p Params) Hash() string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SegmentMeta describes one committed segment file.
type SegmentMeta struct {
	File   string `json:"file"`
	Slice  int64  `json:"slice"`
	Events int    `json:"events"`
	SHA256 string `json:"sha256"`
}

// Manifest is the archive index written next to the segments.
type Manifest struct {
	Params      Params        `json:"params"`
	ParamsHash  string        `json:"params_hash"`
	InputDigest string        `json:"input_digest"`
	Segments    []SegmentMeta `json:"segments"`
}

// Segmenter groups committed events into slices of SliceUs and writes
// each slice atomically once the watermark has passed its end plus the
// rollback horizon, so closed segments are immutable. Open slices are
// kept in a small map (bounded by horizon/slice duration) so a late
// event can still join an older open slice after a rollback.
type Segmenter struct {
	dir     string
	sliceUs media.TimeUs
	horizon media.TimeUs
	reuse   map[int64]string // slice -> expected hash (resume mode)

	bufs map[int64][]timeline.Event
	segs []SegmentMeta

	Reused        int // slices matched against existing files
	ReuseMismatch int // slices whose fresh hash differs from disk
}

func NewSegmenter(dir string, sliceUs, horizon media.TimeUs, reuse map[int64]string) *Segmenter {
	return &Segmenter{
		dir: dir, sliceUs: sliceUs, horizon: horizon, reuse: reuse,
		bufs: map[int64][]timeline.Event{},
	}
}

// Emit appends one committed event to its slice buffer.
func (s *Segmenter) Emit(ev timeline.Event) {
	idx := int64(ev.TimeUs / s.sliceUs)
	s.bufs[idx] = append(s.bufs[idx], ev)
}

// OnWatermark closes slices that can no longer be rolled back.
func (s *Segmenter) OnWatermark(wm media.TimeUs) {
	for idx := range s.bufs {
		if (idx+1)*int64(s.sliceUs)+int64(s.horizon) <= int64(wm) {
			s.close(idx)
		}
	}
}

// Flush closes all remaining open slices at end of stream.
func (s *Segmenter) Flush() {
	idxs := make([]int64, 0, len(s.bufs))
	for idx := range s.bufs {
		idxs = append(idxs, idx)
	}
	sort.Slice(idxs, func(i, j int) bool { return idxs[i] < idxs[j] })
	for _, idx := range idxs {
		s.close(idx)
	}
}

// serialize renders a slice deterministically.
func serialize(slice int64, events []timeline.Event) []byte {
	out := []byte("ARC1")
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], uint64(slice))
	out = append(out, tmp[:]...)
	binary.BigEndian.PutUint32(tmp[:4], uint32(len(events)))
	out = append(out, tmp[:4]...)
	for _, ev := range events {
		kind := byte(0)
		if ev.Track.Kind == media.TrackVideo {
			kind = 1
		}
		out = append(out, kind)
		binary.BigEndian.PutUint32(tmp[:4], ev.Track.SSRC)
		out = append(out, tmp[:4]...)
		binary.BigEndian.PutUint64(tmp[:], uint64(ev.TimeUs))
		out = append(out, tmp[:]...)
		binary.BigEndian.PutUint64(tmp[:], ev.ExtSeq)
		out = append(out, tmp[:]...)
		binary.BigEndian.PutUint32(tmp[:4], uint32(len(ev.Data)))
		out = append(out, tmp[:4]...)
		out = append(out, ev.Data...)
	}
	return out
}

func (s *Segmenter) close(idx int64) {
	events := s.bufs[idx]
	delete(s.bufs, idx)
	content := serialize(idx, events)
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	name := fmt.Sprintf("segment_%06d.bin", idx)
	if want, ok := s.reuse[idx]; ok {
		// Resume mode: keep the existing file, verify reproducibility.
		s.Reused++
		if want != hash {
			s.ReuseMismatch++
		}
	} else if err := atomicWrite(filepath.Join(s.dir, name), content); err != nil {
		panic(fmt.Sprintf("write segment: %v", err))
	}
	s.segs = append(s.segs, SegmentMeta{
		File: name, Slice: idx, Events: len(events), SHA256: hash,
	})
}

// atomicWrite writes via a temporary file plus rename.
func atomicWrite(path string, content []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Segments returns the committed segment metadata in slice order.
func (s *Segmenter) Segments() []SegmentMeta {
	out := append([]SegmentMeta(nil), s.segs...)
	sort.Slice(out, func(i, j int) bool { return out[i].Slice < out[j].Slice })
	return out
}

// Committed returns the number of closed segments.
func (s *Segmenter) Committed() int { return len(s.segs) }

// WriteManifest atomically writes the manifest for this run.
func (s *Segmenter) WriteManifest(inputDigest string, p Params) error {
	m := Manifest{
		Params:      p,
		ParamsHash:  p.Hash(),
		InputDigest: inputDigest,
		Segments:    s.Segments(),
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.dir, "manifest.json"), b)
}

// Snapshot implements timeline.Sink for rollback support: only the
// open (not yet closed) slices are captured, since closed slices are
// beyond the rollback horizon by construction.
type snapshot struct {
	Bufs  map[int64][]timeline.Event
	NSegs int
}

func (s *Segmenter) Snapshot() []byte {
	bufs := make(map[int64][]timeline.Event, len(s.bufs))
	for k, v := range s.bufs {
		bufs[k] = append([]timeline.Event(nil), v...)
	}
	b, _ := json.Marshal(snapshot{Bufs: bufs, NSegs: len(s.segs)})
	return b
}

// Restore rolls the open slices back to a checkpoint snapshot.
func (s *Segmenter) Restore(snap []byte) {
	var ss snapshot
	if err := json.Unmarshal(snap, &ss); err != nil {
		panic(fmt.Sprintf("restore segmenter: %v", err))
	}
	s.bufs = ss.Bufs
	if s.bufs == nil {
		s.bufs = map[int64][]timeline.Event{}
	}
	s.segs = s.segs[:ss.NSegs]
}
