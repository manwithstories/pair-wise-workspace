package index

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mediad/internal/ingest"
	"mediad/internal/model"
)

// Source is the minimum a builder needs from a track: its identity, its
// timebase and a way to re-read a frame at an offset.
type Source interface {
	io.ReadSeeker
	TrackID() uint32
	FrameAt(offset int64, size uint64) (*ingest.FrameBox, error)
}

// Build rebuilds the index for src into a temporary file beside path, then
// atomically renames it over the destination. Existing destination content is
// only ever replaced by a complete, validated file; a partial file left by an
// interrupted run is detected and reported via Stats.Replaced.
//
// Every usable frame the scanner reports becomes one entry; bad frames, torn
// tails and conflicts are excluded so an entry can never point at a frame the
// reader would reject. Validation re-resolves each entry against the source
// and fails the build if any entry drifted.
func Build(src Source, path string, frames []model.Frame) (*Stats, error) {
	st := &Stats{}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".index-*.tmp")
	if err != nil {
		return nil, model.Fatal("index.build", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	w, err := NewWriter(tmp)
	if err != nil {
		return nil, model.Fatal("index.build", err)
	}
	for _, fr := range frames {
		if fr.Status == model.FrameDuplicate {
			st.Skipped++ // folded into the earlier identical frame
		}
		if !fr.Status.Usable() {
			if fr.Status != model.FrameDuplicate {
				st.Skipped++
			}
			continue
		}
		e := Entry{
			FrameNo: fr.Meta.FrameNo,
			TrackID: fr.Meta.TrackID,
			PTS:     fr.PTS,
			Offset:  fr.Offset,
			Size:    fr.Size,
			Digest:  EntryDigest(fr.Meta.TrackID, fr.Meta.FrameNo, fr.PTS, fr.Offset, fr.Size, fr.Digest),
		}
		if err := w.Add(e); err != nil {
			return nil, model.Fatal("index.build", err)
		}
	}
	st.Entries = w.Count()
	if err := w.Finish(func(header [16]byte) error {
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		_, err := tmp.Write(header[:])
		return err
	}); err != nil {
		return nil, model.Fatal("index.build", err)
	}
	if err := tmp.Sync(); err != nil {
		return nil, model.Fatal("index.build", err)
	}
	if fi, err := tmp.Stat(); err == nil {
		st.Bytes = fi.Size()
	}
	if err := tmp.Close(); err != nil {
		return nil, model.Fatal("index.build", err)
	}

	// Validate the file we just wrote before publishing it. A fresh index that
	// fails validation must never replace a good one.
	if err := validateFile(tmpName, src, frames); err != nil {
		return nil, err
	}

	// Note whether an old index existed; the caller reports replacement.
	if _, err := os.Stat(path); err == nil {
		if old, dropped, rerr := Open(path); rerr != nil || dropped > 0 || len(old) == 0 {
			st.Replaced = true
			if dropped > 0 {
				st.Truncated = dropped
			}
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return nil, model.Fatal("index.commit", err)
	}
	committed = true
	return st, nil
}

// Open reads an index file, returning its entries and the number of dropped
// (incomplete) trailing entries.
func Open(path string) ([]Entry, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	return ReadAll(f)
}

// validateFile re-reads a written index and resolves every entry against src.
// The expected PTS for each entry comes from the frames the scanner reported,
// never from the frame box itself, so validation cannot agree with a stale
// entry by reading the same wrong value from both sides.
func validateFile(path string, src Source, frames []model.Frame) error {
	entries, dropped, err := Open(path)
	if err != nil {
		return model.Verify("index.validate", err)
	}
	if dropped > 0 {
		return model.Verify("index.validate", fmt.Errorf("%w: %d dropped entries in fresh index", ErrCorruptIndex, dropped))
	}
	ptsByOffset := make(map[int64]int64, len(frames))
	for _, fr := range frames {
		if fr.Status.Usable() {
			ptsByOffset[fr.Offset] = fr.PTS
		}
	}
	if err := Validate(entries, func(e Entry) ([32]byte, int64, error) {
		fb, err := src.FrameAt(e.Offset, uint64(e.Size))
		if err != nil {
			return [32]byte{}, 0, err
		}
		pts, ok := ptsByOffset[e.Offset]
		if !ok {
			return [32]byte{}, 0, fmt.Errorf("no usable frame at %d", e.Offset)
		}
		return model.DigestBytes(fb.Payload), pts, nil
	}); err != nil {
		return model.Verify("index.validate", err)
	}
	return nil
}

// Lookup returns the entry for a frame number using binary search over the
// frame-ordered index. It reports false when the frame is not indexed (for
// example because it was corrupt or truncated).
func Lookup(entries []Entry, frameNo uint64) (Entry, bool) {
	lo, hi := 0, len(entries)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		switch {
		case entries[mid].FrameNo == frameNo:
			return entries[mid], true
		case entries[mid].FrameNo < frameNo:
			lo = mid + 1
		default:
			hi = mid - 1
		}
	}
	return Entry{}, false
}

// ErrNoIndex reports that no index file exists yet.
var ErrNoIndex = errors.New("index: no index file")
