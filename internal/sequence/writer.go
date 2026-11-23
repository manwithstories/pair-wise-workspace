package sequence

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	"medpipe/internal/model"
)

// Validate checks the in-memory frames for the invariants the reader
// enforces, without producing bytes. Writers call it so a sequence never hits
// disk in a state readers would reject.
func Validate(tracks []model.Track, frames map[string][]model.Frame) error {
	if len(tracks) == 0 {
		return model.New(model.KindInput, "sequence.Validate", "no tracks")
	}
	total := 0
	ids := make(map[string]bool, len(tracks))
	for _, tr := range tracks {
		if tr.ID == "" || tr.Timescale == 0 {
			return model.New(model.KindInput, "sequence.Validate", "track with empty ID or zero timescale")
		}
		if ids[tr.ID] {
			return model.New(model.KindInput, "sequence.Validate", fmt.Sprintf("duplicate track %q", tr.ID))
		}
		ids[tr.ID] = true
		var prev uint64
		for i, fr := range frames[tr.ID] {
			if fr.TrackID != tr.ID {
				return model.New(model.KindInvariant, "sequence.Validate", fmt.Sprintf("frame in track %s carries track ID %s", tr.ID, fr.TrackID))
			}
			if i > 0 && fr.Seq <= prev {
				return model.Corruption("sequence.Validate", tr.ID, fr.Seq, 0, "frame sequence not strictly increasing")
			}
			prev = fr.Seq
			if len(fr.Payload) > maxPayloadBytes {
				return model.New(model.KindInput, "sequence.Validate", fmt.Sprintf("frame %d payload too large", fr.Seq))
			}
			if got := fr.IntegrityChecksum(); got != fr.Checksum && fr.Checksum != [32]byte{} {
				return model.Corruption("sequence.Validate", tr.ID, fr.Seq, 0, "in-memory checksum does not match content")
			}
			if len(fr.Meta) > maxMetaEntries {
				return model.New(model.KindInput, "sequence.Validate", fmt.Sprintf("frame %d has too many metadata entries", fr.Seq))
			}
			for _, m := range fr.Meta {
				if m.Key == "" || len(m.Key) > maxStringBytes || len(m.Value) > maxStringBytes {
					return model.New(model.KindInput, "sequence.Validate", fmt.Sprintf("frame %d has invalid metadata", fr.Seq))
				}
			}
			total++
		}
	}
	if total > maxFrames {
		return model.New(model.KindInput, "sequence.Validate", "too many frames")
	}
	return nil
}

// WriteFile writes a sequence atomically to path, creating parent
// directories. The temp file is removed on failure so a partial write is
// never mistaken for a valid sequence.
func WriteFile(path string, tracks []model.Track, frames map[string][]model.Frame) ([32]byte, int64, error) {
	if err := Validate(tracks, frames); err != nil {
		return [32]byte{}, 0, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return [32]byte{}, 0, model.Wrap(model.KindInput, "sequence.WriteFile", err)
	}
	tmp, err := os.CreateTemp(dir, ".medpseq-*")
	if err != nil {
		return [32]byte{}, 0, model.Wrap(model.KindInput, "sequence.WriteFile", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if err := writeSequence(tmp, tracks, frames); err != nil {
		cleanup()
		return [32]byte{}, 0, err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return [32]byte{}, 0, model.Wrap(model.KindInput, "sequence.WriteFile", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return [32]byte{}, 0, model.Wrap(model.KindInput, "sequence.WriteFile", err)
	}
	// Re-read the temp file to prove the bytes parse and to capture the true
	// on-disk digest.
	data, err := os.ReadFile(tmpName)
	if err != nil {
		os.Remove(tmpName)
		return [32]byte{}, 0, model.Wrap(model.KindInput, "sequence.WriteFile", err)
	}
	parsed, err := Parse(data)
	if err != nil {
		os.Remove(tmpName)
		return [32]byte{}, 0, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return [32]byte{}, 0, model.Wrap(model.KindInput, "sequence.WriteFile", err)
	}
	return parsed.Digest, int64(len(data)), nil
}

func writeSequence(w io.Writer, tracks []model.Track, frames map[string][]model.Frame) error {
	h := Header{Version: 1, Tracks: tracks}
	total := 0
	for _, tr := range tracks {
		total += len(frames[tr.ID])
	}
	if total > maxFrames {
		return model.New(model.KindInput, "sequence.Write", "too many frames")
	}
	h.FrameCount = uint32(total)
	hb, err := json.Marshal(h)
	if err != nil {
		return model.Wrap(model.KindInvariant, "sequence.Write", err)
	}
	if len(hb) > maxHeaderBytes {
		return model.New(model.KindInput, "sequence.Write", "header exceeds size limit")
	}
	var b [8]byte
	if _, err := w.Write(magic[:]); err != nil {
		return model.Wrap(model.KindInput, "sequence.Write", err)
	}
	binary.BigEndian.PutUint32(b[:4], uint32(len(hb)))
	if _, err := w.Write(b[:4]); err != nil {
		return model.Wrap(model.KindInput, "sequence.Write", err)
	}
	if _, err := w.Write(hb); err != nil {
		return model.Wrap(model.KindInput, "sequence.Write", err)
	}
	trackIndex := make(map[string]uint32, len(tracks))
	for i, tr := range tracks {
		trackIndex[tr.ID] = uint32(i)
	}
	for _, tr := range tracks {
		idx := trackIndex[tr.ID]
		for _, fr := range frames[tr.ID] {
			if err := writeFrame(w, idx, tr.ID, fr); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeFrame(w io.Writer, trackIdx uint32, trackID string, fr model.Frame) error {
	if fr.TrackID != "" && fr.TrackID != trackID {
		return model.New(model.KindInvariant, "sequence.Write", fmt.Sprintf("frame %d track mismatch", fr.Seq))
	}
	if len(fr.Energy) > 4096 {
		return model.New(model.KindInput, "sequence.Write", fmt.Sprintf("frame %d has too many energy samples", fr.Seq))
	}
	var rec bytes.Buffer
	var b [8]byte
	binary.BigEndian.PutUint32(b[:4], trackIdx)
	rec.Write(b[:4])
	binary.BigEndian.PutUint64(b[:], fr.Seq)
	rec.Write(b[:])
	binary.BigEndian.PutUint64(b[:], fr.RawTS)
	rec.Write(b[:])
	if fr.Keyframe {
		rec.WriteByte(1)
	} else {
		rec.WriteByte(0)
	}
	binary.BigEndian.PutUint16(b[:2], fr.Width)
	rec.Write(b[:2])
	binary.BigEndian.PutUint16(b[:2], fr.Height)
	rec.Write(b[:2])
	binary.BigEndian.PutUint16(b[:2], uint16(len(fr.Energy)))
	rec.Write(b[:2])
	binary.BigEndian.PutUint64(b[:], uint64(len(fr.Payload)))
	rec.Write(b[:])
	rec.Write(fr.Payload)
	for _, e := range fr.Energy {
		binary.BigEndian.PutUint64(b[:], math.Float64bits(e))
		rec.Write(b[:])
	}
	binary.BigEndian.PutUint16(b[:2], uint16(len(fr.Meta)))
	rec.Write(b[:2])
	for _, m := range fr.Meta {
		if m.Key == "" || len(m.Key) > maxStringBytes || len(m.Value) > maxStringBytes {
			return model.New(model.KindInput, "sequence.Write", fmt.Sprintf("frame %d has invalid metadata", fr.Seq))
		}
		binary.BigEndian.PutUint16(b[:2], uint16(len(m.Key)))
		rec.Write(b[:2])
		binary.BigEndian.PutUint16(b[:2], uint16(len(m.Value)))
		rec.Write(b[:2])
		rec.WriteString(m.Key)
		rec.WriteString(m.Value)
	}
	// Recompute rather than trust the caller; a frame written with a stale
	// checksum would be rejected by every reader.
	sum := fr.IntegrityChecksum()
	rec.Write(sum[:])
	if _, err := w.Write(rec.Bytes()); err != nil {
		return model.Wrap(model.KindInput, "sequence.Write", err)
	}
	return nil
}

func float64Bits(v float64) uint64 { return math.Float64bits(v) }
func float64FromBits(v uint64) float64 {
	return math.Float64frombits(v)
}
