package index

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mediad/internal/model"
)

// FrameRef is the minimal per-frame record the builder needs. Frames are
// supplied through a reader rather than a slice so a track of any length can
// be indexed with memory bounded by one record.
type FrameRef struct {
	FrameNo uint64
	TrackID uint32
	PTS     int64
	Offset  int64
	Size    uint32
	Content [32]byte
	Status  model.FrameStatus
}

// FrameSource yields the next frame record, returning io.EOF at the end.
type FrameSource interface {
	Next() (FrameRef, error)
}

// StreamStats describes a built index.
type StreamStats struct {
	Entries int
	Skipped int // unusable frames deliberately excluded
	Bytes   int64
}

// ErrNoFrames reports a source that produced no frame at all.
var ErrNoFrames = fmt.Errorf("%w: no frames", ErrCorruptIndex)

// BuildStream consumes frames in ascending order and writes the index to path
// atomically. Each entry is validated against the frame reference that
// produced it (digest recomputation and monotonic PTS), so a wrong entry
// cannot be written in the first place.
func BuildStream(src FrameSource, path string) (*StreamStats, error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".index-*.tmp")
	if err != nil {
		return nil, model.Fatal("index.build", err)
	}
	name := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(name)
		}
	}()

	st := &StreamStats{}
	w, err := NewWriter(tmp)
	if err != nil {
		return nil, model.Fatal("index.build", err)
	}
	var lastPTS int64 = -1
	for {
		fr, err := src.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if !fr.Status.Usable() {
			st.Skipped++
			continue
		}
		if fr.PTS < lastPTS {
			return nil, model.Verify("index.build", fmt.Errorf("frame %d: pts %d after %d", fr.FrameNo, fr.PTS, lastPTS))
		}
		lastPTS = fr.PTS
		e := Entry{
			FrameNo: fr.FrameNo, TrackID: fr.TrackID, PTS: fr.PTS,
			Offset: fr.Offset, Size: fr.Size,
			Digest: EntryDigest(fr.TrackID, fr.FrameNo, fr.PTS, fr.Offset, fr.Size, fr.Content),
		}
		// Self-check before writing: the entry digest must resolve back from
		// the same inputs, catching any encoding drift immediately.
		if got := EntryDigest(e.TrackID, e.FrameNo, e.PTS, e.Offset, e.Size, fr.Content); got != e.Digest {
			return nil, model.Verify("index.build", fmt.Errorf("frame %d: entry digest does not round-trip", fr.FrameNo))
		}
		if err := w.Add(e); err != nil {
			return nil, model.Fatal("index.build", err)
		}
	}
	if w.Count() == 0 {
		return nil, model.Verify("index.build", ErrNoFrames)
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
	// Re-read what we wrote and confirm it is complete before publishing.
	if _, dropped, err := Open(name); err != nil || dropped > 0 {
		if err == nil {
			err = fmt.Errorf("%w: %d dropped entries", ErrCorruptIndex, dropped)
		}
		return nil, model.Verify("index.validate", err)
	}
	if err := os.Rename(name, path); err != nil {
		return nil, model.Fatal("index.commit", err)
	}
	committed = true
	return st, nil
}

// Scan reads an index file and invokes fn for each entry, in order, without
// materializing the full slice. It reports dropped trailing entries.
func Scan(r io.ReadSeeker, fn func(Entry) error) (dropped int, err error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	head := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, head); err != nil {
		return 0, fmt.Errorf("%w: short header", ErrCorruptIndex)
	}
	br := bufio.NewReader(r)
	for {
		var raw [EntrySize]byte
		n, err := io.ReadFull(br, raw[:])
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				if n > 0 {
					dropped++
				}
				return dropped, nil
			}
			return 0, err
		}
		e, ok := decodeEntry(raw[:])
		if !ok {
			return dropped + 1, nil
		}
		if err := fn(e); err != nil {
			return dropped, err
		}
	}
}

// HeaderDigest returns a digest over the entry digests written so far. It is a
// stable identifier of the index content, independent of file framing.
func (i *Writer) HeaderDigest() [32]byte {
	return model.DigestBytes(i.entryDigests)
}
