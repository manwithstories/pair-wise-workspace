// Package hash reads compressed samples at their file offsets and
// computes per-frame SHA-256 digests with bounded concurrency.
package hash

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sync"

	"mp4loc/internal/box"
	"mp4loc/internal/time"
)

// Record is one hashed sample.
type Record struct {
	Index     int
	Offset    int64
	Size      uint32
	DTS       int64 // raw media-time DTS
	PTS       int64 // raw media-time PTS
	DTSNorm   int64 // wrap-normalized DTS
	PTSNorm   int64 // wrap-normalized PTS
	Keyframe  bool
	FrameType string
	SHA256    string // 64-char hex; empty when hashing disabled
}

// maxSampleBytes caps a single sample read (512 MiB).
const maxSampleBytes = 1 << 29

// Samples builds records for the given sample indices, reading and
// hashing each sample's compressed bytes when doHash is set. Reads are
// validated against fileSize and run with at most workers goroutines.
func Samples(ra io.ReaderAt, fileSize int64, tr *box.Track, tl *timeline.Timeline,
	indices []int, doHash bool, workers int, frameType func(*box.SampleRecord) string) ([]Record, error) {

	if workers < 1 {
		workers = 1
	}
	recs := make([]Record, len(indices))
	for i, idx := range indices {
		s := tr.Table.Samples[idx]
		if s.Offset < 0 || s.Offset+int64(s.Size) > fileSize {
			return nil, &box.StructureError{
				Offset: s.Offset,
				Reason: "sample offset/size out of file bounds",
			}
		}
		if int64(s.Size) > maxSampleBytes {
			return nil, &box.StructureError{Offset: s.Offset, Reason: "sample size exceeds safety cap"}
		}
		dtsRaw, dtsNorm := tl.Normalize(s.DTS)
		_, ptsNorm := tl.Normalize(s.PTS)
		ptsRaw := dtsRaw + (s.PTS - s.DTS)
		recs[i] = Record{
			Index:     idx,
			Offset:    s.Offset,
			Size:      s.Size,
			DTS:       dtsRaw,
			PTS:       ptsRaw,
			DTSNorm:   dtsNorm,
			PTSNorm:   ptsNorm,
			Keyframe:  s.Keyframe,
			FrameType: frameType(&tr.Table.Samples[idx]),
		}
	}
	if !doHash {
		return recs, nil
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	var mu sync.Mutex
	var firstErr error
	for i := range recs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			buf := make([]byte, recs[i].Size)
			if _, err := ra.ReadAt(buf, recs[i].Offset); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = &box.StructureError{Offset: recs[i].Offset, Reason: "sample read failed: " + err.Error()}
				}
				mu.Unlock()
				return
			}
			sum := sha256.Sum256(buf)
			recs[i].SHA256 = hex.EncodeToString(sum[:])
		}(i)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return recs, nil
}
