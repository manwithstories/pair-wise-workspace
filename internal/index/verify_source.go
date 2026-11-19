package index

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"

	"mediq/internal/model"
	"mediq/internal/source"
	"mediq/internal/validate"
)

// verifyAgainstSource proves the index is an order-preserving projection of
// the source's valid frames:
//
//   - every indexed sample matches the SHA-256 of a source frame that appears
//     later in the source stream than the previous sample's frame, so samples
//     can never be reordered or substituted;
//   - every source frame failing validation is absent from the index.
//
// Valid source frames the timeline stage quarantined are legitimately absent;
// they are counted in UnindexedSource rather than treated as corruption.
func verifyAgainstSource(mp4File io.ReaderAt, t *Table, sourcePath string, rep *VerifyReport) error {
	src, err := source.Open(sourcePath, source.Options{})
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer src.Close()

	type goodFrame struct {
		seq  uint64
		hash [32]byte
	}
	// nextGood returns the next valid, first-seen frame from the source.
	seen := make(map[uint64]struct{}, 256)
	window := make([]uint64, 0, 256)
	nextGood := func() (*goodFrame, bool, error) {
		for {
			rec, err := src.Read()
			if err == io.EOF {
				return nil, false, nil
			}
			if err != nil {
				return nil, false, fmt.Errorf("read source: %w", err)
			}
			if rec.Fault != nil {
				rep.SourceFaults = append(rep.SourceFaults, rec.Fault)
				continue
			}
			fr := rec.Frame
			if f := validate.CheckFrame(fr); f != nil {
				rep.SourceFaults = append(rep.SourceFaults, f)
				continue
			}
			if _, dup := seen[fr.Seq]; dup {
				rep.SourceFaults = append(rep.SourceFaults, &model.Fault{
					Code: model.CodeDuplicateSeq, Seq: fr.Seq, HasSeq: true,
					Offset: fr.FrameOffset, Reason: "duplicate sequence number skipped",
				})
				continue
			}
			seen[fr.Seq] = struct{}{}
			window = append(window, fr.Seq)
			if len(window) > 8192 {
				delete(seen, window[0])
				window = window[1:]
			}
			return &goodFrame{seq: fr.Seq, hash: fr.Hash}, true, nil
		}
	}

	sum := sha256.New()
	pending, havePending, err := nextGood()
	if err != nil {
		return err
	}
	for sample := 0; sample < len(t.SampleSize); sample++ {
		sum.Reset()
		if _, err := io.Copy(sum, io.NewSectionReader(mp4File,
			int64(t.Offsets[sample]), int64(t.SampleSize[sample]))); err != nil {
			return fmt.Errorf("read sample %d: %w", sample, err)
		}
		want := sum.Sum(nil)

		matched := false
		skipped := 0
		for havePending {
			if bytes.Equal(want, pending.hash[:]) {
				matched = true
				pending, havePending, err = nextGood()
				if err != nil {
					return err
				}
				break
			}
			skipped++
			pending, havePending, err = nextGood()
			if err != nil {
				return err
			}
		}
		rep.UnindexedSource += skipped
		if !matched {
			rep.Problems = append(rep.Problems,
				fmt.Sprintf("index sample %d has no matching source frame in remaining input", sample))
			if len(rep.Problems) >= 16 {
				return nil
			}
		}
	}
	// Everything valid left over was not indexed.
	for havePending {
		rep.UnindexedSource++
		pending, havePending, err = nextGood()
		if err != nil {
			return err
		}
	}
	return nil
}
