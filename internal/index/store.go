package index

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrInconsistent marks index self-consistency failures (exit code 4).
var ErrInconsistent = errors.New("index consistency error")

func inconsistentf(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrInconsistent, fmt.Sprintf(format, args...))
}

// Save writes the index as compact JSON. Tracks are marshalled one at a
// time so a 14-hour file does not require the whole document in memory.
func Save(idx *Index, w io.Writer) error {
	bw := bufio.NewWriterSize(w, 1<<20)
	head, err := json.Marshal(struct {
		Version      int    `json:"version"`
		File         string `json:"file"`
		Size         uint64 `json:"size"`
		MoovOffset   uint64 `json:"moov_offset"`
		MdatOffset   uint64 `json:"mdat_offset"`
		MdatEnd      uint64 `json:"mdat_end"`
		UnknownBoxes int    `json:"unknown_boxes"`
	}{idx.Version, idx.File, idx.Size, idx.MoovOffset, idx.MdatOffset, idx.MdatEnd, idx.UnknownBoxes})
	if err != nil {
		return err
	}
	// Splice the tracks array in after the header object.
	if _, err := bw.Write(head[:len(head)-1]); err != nil {
		return err
	}
	if _, err := bw.WriteString(`,"tracks":[`); err != nil {
		return err
	}
	for i, t := range idx.Tracks {
		if i > 0 {
			if err := bw.WriteByte(','); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(t)
		if err != nil {
			return err
		}
		if _, err := bw.Write(raw); err != nil {
			return err
		}
	}
	if _, err := bw.WriteString(`]}`); err != nil {
		return err
	}
	if _, err := bw.WriteString("\n"); err != nil {
		return err
	}
	return bw.Flush()
}

// Load reads an index file back.
func Load(r io.Reader) (*Index, error) {
	var idx Index
	dec := json.NewDecoder(bufio.NewReaderSize(r, 1<<20))
	if err := dec.Decode(&idx); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInconsistent, err)
	}
	return &idx, nil
}

// Validate checks the index against itself and the file size: array
// lengths agree, DTS is monotonic, sync indices are sorted and in
// range, and every sample lies inside the recorded mdat extent.
func (idx *Index) Validate() error {
	if idx.Version != Version {
		return inconsistentf("unsupported version %d", idx.Version)
	}
	if idx.MdatEnd < idx.MdatOffset || idx.MdatEnd > idx.Size {
		return inconsistentf("mdat extent [%d,%d) inconsistent with size %d",
			idx.MdatOffset, idx.MdatEnd, idx.Size)
	}
	for _, t := range idx.Tracks {
		if err := validateTrack(t, idx.MdatOffset, idx.MdatEnd); err != nil {
			return err
		}
	}
	return nil
}

func validateTrack(t *TrackIndex, mdatStart, mdatEnd uint64) error {
	n := t.SampleCount
	if len(t.Offsets) != n || len(t.Sizes) != n || len(t.PTS) != n || len(t.DTS) != n {
		return inconsistentf("track %d: array lengths %d/%d/%d/%d != sample_count %d",
			t.TrackID, len(t.Offsets), len(t.Sizes), len(t.PTS), len(t.DTS), n)
	}
	for i := 1; i < n; i++ {
		if t.DTS[i] < t.DTS[i-1] {
			return inconsistentf("track %d: dts not monotonic at sample %d", t.TrackID, i)
		}
	}
	prev := -1
	for _, s := range t.Sync {
		if int(s) >= n {
			return inconsistentf("track %d: sync index %d >= sample_count %d", t.TrackID, s, n)
		}
		if int(s) <= prev {
			return inconsistentf("track %d: sync indices not strictly increasing", t.TrackID)
		}
		prev = int(s)
	}
	if !t.AllSync && len(t.Sync) != t.SyncSampleCount {
		return inconsistentf("track %d: sync_sample_count %d != %d", t.TrackID, t.SyncSampleCount, len(t.Sync))
	}
	for i := 0; i < n; i++ {
		off := uint64(t.Offsets[i])
		if off < mdatStart || off+uint64(t.Sizes[i]) > mdatEnd {
			return inconsistentf("track %d: sample %d [%d,%d) outside mdat [%d,%d)",
				t.TrackID, i, off, off+uint64(t.Sizes[i]), mdatStart, mdatEnd)
		}
	}
	return nil
}
