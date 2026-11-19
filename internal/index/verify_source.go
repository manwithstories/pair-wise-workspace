package index

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"

	"mediq/internal/model"
	"mediq/internal/source"
	"mediq/internal/validate"
)

// verifyAgainstSourceStream proves the index is an order-preserving projection
// of the source's valid frames:
//
//   - every indexed sample matches the SHA-256 of a source frame that appears
//     later in the source stream than the previous sample's frame, so samples
//     can never be reordered or substituted;
//   - every source frame failing validation is absent from the index.
//
// Both the index and the source are consumed as streams, so verification uses
// constant memory regardless of input length.
func verifyAgainstSourceStream(f *os.File, rep *VerifyReport, sourcePath string) error {
	ts, err := openTableSource(f)
	if err != nil {
		rep.Problems = append(rep.Problems, "index tables unreadable: "+err.Error())
		return nil
	}
	src, err := source.Open(sourcePath, source.Options{})
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer src.Close()

	// nextGood returns the next valid, first-seen frame from the source.
	seen := make(map[uint64]struct{}, 256)
	window := make([]uint64, 0, 256)
	nextGood := func() (*model.Frame, error) {
		for {
			rec, err := src.Read()
			if err == io.EOF {
				return nil, nil
			}
			if err != nil {
				return nil, fmt.Errorf("read source: %w", err)
			}
			if rec.Fault != nil {
				rep.SourceFaults = append(rep.SourceFaults, rec.Fault)
				continue
			}
			fr := rec.Frame
			if fault := validate.CheckFrame(fr); fault != nil {
				rep.SourceFaults = append(rep.SourceFaults, fault)
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
			return fr, nil
		}
	}

	sum := sha256.New()
	readBuf := make([]byte, 64<<10)
	pending, err := nextGood()
	if err != nil {
		return err
	}
	var sample uint64
	missing := false
	for {
		off, size, _, ok, err := ts.next()
		if err != nil {
			rep.Problems = append(rep.Problems, "index tables unreadable: "+err.Error())
			return nil
		}
		if !ok {
			break
		}
		// Read the sample with a positional read: the table readers and the
		// digest share the file, so a shared seek position would corrupt one
		// another.
		if cap(readBuf) < int(size) {
			readBuf = make([]byte, size)
		}
		buf := readBuf[:size]
		if _, err := f.ReadAt(buf, int64(off)); err != nil {
			return fmt.Errorf("read sample %d: %w", sample, err)
		}
		sum.Reset()
		sum.Write(buf)
		want := sum.Sum(nil)

		matched := false
		for pending != nil {
			if bytes.Equal(want, pending.Hash[:]) {
				matched = true
				pending, err = nextGood()
				if err != nil {
					return err
				}
				break
			}
			rep.UnindexedSource++
			pending, err = nextGood()
			if err != nil {
				return err
			}
		}
		if !matched {
			missing = true
			rep.Problems = append(rep.Problems,
				fmt.Sprintf("index sample %d has no matching source frame in remaining input", sample))
			if len(rep.Problems) >= 16 {
				return nil
			}
		}
		sample++
	}
	for pending != nil {
		rep.UnindexedSource++
		pending, err = nextGood()
		if err != nil {
			return err
		}
	}
	_ = missing
	return nil
}
