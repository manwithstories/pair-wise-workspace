package pipeline

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"mediad/internal/checkpoint"
	"mediad/internal/index"
	"mediad/internal/ingest"
	"mediad/internal/metrics"
	"mediad/internal/model"
)

// prepareResume loads and verifies a checkpoint. A checkpoint that fails
// verification is ignored, never trusted; the run restarts clean.
func prepareResume(paths Paths, want bool) (*ingest.Resume, checkpoint.State, error) {
	if !want {
		return nil, checkpoint.State{}, nil
	}
	st, err := checkpoint.Load(paths.State())
	if err != nil {
		return nil, checkpoint.State{}, nil
	}
	if err := checkpoint.Verify(st); err != nil {
		return nil, checkpoint.State{}, nil
	}
	if st.Stage == checkpoint.StageIngest && st.NextFrame > 0 {
		return &ingest.Resume{
			Offset: st.Offset, NextFrame: st.NextFrame,
			Mapper: st.Mapper, Sequencer: st.Sequencer,
		}, st, nil
	}
	return nil, checkpoint.State{}, nil
}

// commitIngestCheckpoint fsyncs the streaming writers and saves ingest-stage
// state naming the source and the index prefix. The manifest is not yet
// durable by digest at this stage, which Ordering permits.
func commitIngestCheckpoint(cur *ingest.Cursor, paths Paths, trackID uint32, next uint64, st *streamState, idxTmp, logf *os.File, entries int) error {
	if err := idxTmp.Sync(); err != nil {
		return model.Fatal("pipeline.checkpoint", err)
	}
	// The frames log must be durable before its digest is recorded; otherwise a
	// restart could verify a checkpoint whose frames were never written.
	if err := logf.Sync(); err != nil {
		return model.Fatal("pipeline.checkpoint", err)
	}
	srcArt, err := checkpoint.ArtifactOf(paths.Source())
	if err != nil {
		return model.Fatal("pipeline.checkpoint", err)
	}
	framesArt, err := checkpoint.ArtifactOf(paths.FramesLog())
	if err != nil {
		return model.Fatal("pipeline.checkpoint", err)
	}
	mapper, seq := cur.Snapshot()
	return checkpoint.Save(paths.State(), checkpoint.State{
		TrackID: trackID, NextFrame: next, Indexed: uint64(entries), Offset: cur.Offset(),
		FramesOK: uint64(st.ok), FramesBad: uint64(st.bad), Stage: checkpoint.StageIngest,
		Source: srcArt, FramesFile: framesArt, Mapper: mapper, Sequencer: seq,
	})
}

func saveFinal(paths Paths, trackID uint32, res *TrackResult) error {
	srcArt, err := checkpoint.ArtifactOf(paths.Source())
	if err != nil {
		return model.Fatal("pipeline.finalize", err)
	}
	idxArt, err := checkpoint.ArtifactOf(paths.Index())
	if err != nil {
		return model.Fatal("pipeline.finalize", err)
	}
	st, err := checkpoint.Load(paths.State())
	if err != nil {
		st = checkpoint.State{}
	}
	return checkpoint.Save(paths.State(), checkpoint.State{
		TrackID: trackID, NextFrame: st.NextFrame, Offset: st.Offset,
		FramesOK: st.FramesOK, FramesBad: st.FramesBad, Stage: checkpoint.StageManifest,
		Source: srcArt, FramesFile: st.FramesFile, IndexFile: idxArt,
		ManifestSum: fmt.Sprintf("%x", res.ManifestSum),
	})
}

// trimToResume truncates the frames log to the checkpoint's frame count and
// returns how many index entries the durable prefix should contain. The index
// file itself is not read: it may be torn, and this run rebuilds it.
func trimToResume(paths Paths, resume *ingest.Resume) (int, error) {
	if resume == nil {
		if err := os.Truncate(paths.FramesLog(), 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		return 0, nil
	}
	want := int64(resume.NextFrame) * frameFactSize
	f, err := os.OpenFile(paths.FramesLog(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if fi.Size() > want {
		if err := f.Truncate(want); err != nil {
			return 0, err
		}
	}
	return int(resume.NextFrame), nil
}

// rebuildPrefixReader returns a pull iterator over index entries for the
// durable prefix, re-deriving each from the frame facts and the source. It
// counts only usable frames, matching the prefix count the caller expects.
func rebuildPrefixReader(paths Paths, resume *ingest.Resume) (func() (index.Entry, bool, error), error) {
	if resume == nil {
		return func() (index.Entry, bool, error) { return index.Entry{}, false, nil }, nil
	}
	src, err := os.Open(paths.Source())
	if err != nil {
		return nil, err
	}
	logf, err := os.Open(paths.FramesLog())
	if err != nil {
		src.Close()
		return nil, err
	}
	buf := make([]byte, frameFactSize)
	remaining := int(resume.NextFrame)
	return func() (index.Entry, bool, error) {
		for remaining > 0 {
			remaining--
			if _, err := io.ReadFull(logf, buf); err != nil {
				logf.Close()
				src.Close()
				return index.Entry{}, false, nil
			}
			rec, ok := decodeFrameFact(buf)
			if !ok || !model.FrameStatus(rec.Status).Usable() {
				continue
			}
			fb, err := ingest.ReadFrameAt(src, rec.Offset, uint64(rec.Size))
			if err != nil {
				continue
			}
			content := model.DigestBytes(fb.Payload)
			return index.Entry{
				FrameNo: fb.FrameNo, TrackID: fb.TrackID, PTS: rec.PTS,
				Offset: rec.Offset, Size: rec.Size,
				Digest: index.EntryDigest(fb.TrackID, fb.FrameNo, rec.PTS, rec.Offset, rec.Size, content),
			}, true, nil
		}
		logf.Close()
		src.Close()
		return index.Entry{}, false, nil
	}, nil
}

type frameFact struct {
	FrameNo uint64
	Offset  int64
	Size    uint32
	PTS     int64
	Status  byte
}

func decodeFrameFact(b []byte) (frameFact, bool) {
	if len(b) < frameFactSize {
		return frameFact{}, false
	}
	return frameFact{
		FrameNo: binary.BigEndian.Uint64(b[0:]),
		Offset:  int64(binary.BigEndian.Uint64(b[8:])),
		Size:    binary.BigEndian.Uint32(b[16:]),
		PTS:     int64(binary.BigEndian.Uint64(b[20:])),
		Status:  b[28],
	}, true
}

// guardUnfinished refuses to overwrite published output when no verified
// checkpoint proves it was completed. The frames log and checkpoint itself are
// not outputs; the index and manifest are. A fresh track with neither passes.
func guardUnfinished(paths Paths) error {
	hasOutput := false
	for _, p := range []string{paths.Index(), paths.Manifest()} {
		if _, err := os.Stat(p); err == nil {
			hasOutput = true
			break
		}
	}
	if !hasOutput {
		return nil
	}
	// Output exists. It is safe to replace only when a committed checkpoint
	// verifies that the previous run's artifacts are intact; otherwise this
	// would overwrite output whose completion was never proven.
	st, err := checkpoint.Load(paths.State())
	if err != nil {
		return model.Fatal("pipeline.guard", fmt.Errorf("%w: no committed checkpoint", model.ErrUnfinishedOutput))
	}
	if err := checkpoint.Verify(st); err != nil {
		return model.Fatal("pipeline.guard", fmt.Errorf("%w: %v", model.ErrUnfinishedOutput, err))
	}
	return nil
}

// encodeFrameFact serializes one frame's durable fact record.
func encodeFrameFact(fr model.Frame) []byte {
	b := make([]byte, 0, frameFactSize)
	b = binary.BigEndian.AppendUint64(b, fr.Meta.FrameNo)
	b = binary.BigEndian.AppendUint64(b, uint64(fr.Offset))
	b = binary.BigEndian.AppendUint32(b, fr.Size)
	b = binary.BigEndian.AppendUint64(b, uint64(fr.PTS))
	b = append(b, byte(fr.Status))
	return b
}

// countIndex reports how many complete entries an index file holds.
func countIndex(path string) int {
	entries, dropped, err := index.Open(path)
	if err != nil {
		return 0
	}
	return len(entries) - dropped
}

const frameFactSize = 8 + 8 + 4 + 8 + 1

// streamState is the O(1) running state shared by the pass.
type streamState struct {
	track    *ingest.Track
	frames   int
	ok       int
	bad      int
	lastPTS  int64
	firstPTS int64
	havePTS  bool
	cache    metrics.Counter
}
