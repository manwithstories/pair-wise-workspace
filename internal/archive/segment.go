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
// rollback horizon, so closed segments are immutable.
type Segmenter struct {
	dir      string
	sliceUs  media.TimeUs
	horizon  media.TimeUs
	reuse    map[int64]string // slice -> expected hash (resume mode)

	started bool
	curIdx  int64
	cur     []timeline.Event
	segs    []SegmentMeta

	Reused        int // slices matched against existing files
	ReuseMismatch int // slices whose fresh hash differs from disk
}

func NewSegmenter(dir string, sliceUs, horizon media.TimeUs, reuse map[int64]string) *Segmenter {
	return &Segmenter{dir: dir, sliceUs: sliceUs, horizon: horizon, reuse: reuse}
}

// Emit appends one committed event to its slice buffer.
func (s *Segmenter) Emit(ev timeline.Event) {
	idx := int64(ev.TimeUs / s.sliceUs)
	if !s.started {
		s.started, s.curIdx = true, idx
	}
	if idx != s.curIdx {
		// Committed order is non-decreasing; a newer slice starts here.
		s.close()
		s.curIdx = idx
	}
	s.cur = append(s.cur, ev)
}

// OnWatermark closes slices that can no longer be rolled back.
func (s *Segmenter) OnWatermark(wm media.TimeUs) {
	if s.started && (s.curIdx+1)*int64(s.sliceUs)+int64(s.horizon) <= int64(wm) {
		s.close()
	}
}

// Flush closes the final open slice at end of stream.
func (s *Segmenter) Flush() {
	if s.started && len(s.cur) > 0 {
		s.close()
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

func (s *Segmenter) close() {
	content := serialize(s.curIdx, s.cur)
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	name := fmt.Sprintf("segment_%06d.bin", s.curIdx)
	if want, ok := s.reuse[s.curIdx]; ok {
		// Resume mode: keep the existing file, verify reproducibility.
		s.Reused++
		if want != hash {
			s.ReuseMismatch++
		}
	} else if err := atomicWrite(filepath.Join(s.dir, name), content); err != nil {
		panic(fmt.Sprintf("write segment: %v", err))
	}
	s.segs = append(s.segs, SegmentMeta{
		File: name, Slice: s.curIdx, Events: len(s.cur), SHA256: hash,
	})
	s.cur = nil
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
// open (not yet closed) slice is captured, since closed slices are
// beyond the rollback horizon by construction.
type snapshot struct {
	Started bool
	CurIdx  int64
	Cur     []timeline.Event
	NSegs   int
}

func (s *Segmenter) Snapshot() []byte {
	b, _ := json.Marshal(snapshot{
		Started: s.started, CurIdx: s.curIdx, Cur: s.cur, NSegs: len(s.segs),
	})
	return b
}

// Restore rolls the open slice back to a checkpoint snapshot.
func (s *Segmenter) Restore(snap []byte) {
	var ss snapshot
	if err := json.Unmarshal(snap, &ss); err != nil {
		panic(fmt.Sprintf("restore segmenter: %v", err))
	}
	s.started, s.curIdx, s.cur = ss.Started, ss.CurIdx, ss.Cur
	s.segs = s.segs[:ss.NSegs]
}
