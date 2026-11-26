package bmff

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"

	"mediad/internal/model"
)

// ScanFile hashes the complete input and builds a located sample index. The
// input is opened read-only and is never modified.
func ScanFile(path string) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, model.Wrap(model.CodeInvalidArgument, model.StageScan, "open input", err).
			WithSource(path)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, model.Wrap(model.CodeInternal, model.StageScan, "stat input", err).WithSource(path)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, model.Wrap(model.CodeInternal, model.StageScan, "hash input", err).WithSource(path)
	}
	r := NewReader(path, f, st.Size())
	idx, err := r.Scan(hex.EncodeToString(h.Sum(nil)))
	if err != nil {
		return nil, err
	}
	return idx, nil
}

func (s *Reader) Scan(inputSHA string) (*Index, error) {
	tops, err := s.Children(nil)
	if err != nil {
		return nil, err
	}
	var moov *BoxHeader
	var moofs []BoxHeader
	for i := range tops {
		switch tops[i].Type {
		case "moov":
			moov = &tops[i]
		case "moof":
			moofs = append(moofs, tops[i])
		}
	}
	if moov == nil {
		return nil, model.New(model.CodeCorruptBox, model.StageScan, "moov box not found").
			WithSource(s.Source).WithBox("moov")
	}
	moovChildren, err := s.Children(moov)
	if err != nil {
		return nil, err
	}
	idx := &Index{Version: IndexVersion, InputSHA256: inputSHA, SourceSize: s.Size}
	for i := range moovChildren {
		if moovChildren[i].Type != "trak" {
			continue
		}
		t, err := s.parseTrak(&moovChildren[i], len(moofs) == 0)
		if err != nil {
			return nil, err
		}
		idx.Tracks = append(idx.Tracks, *t)
	}
	if len(idx.Tracks) == 0 {
		return nil, model.New(model.CodeCorruptBox, model.StageScan, "moov contains no trak").
			WithSource(s.Source).WithBox("trak")
	}
	if len(moofs) > 0 {
		idx.Fragmented = true
		if err := s.parseFragments(idx, moofs); err != nil {
			return nil, err
		}
	}
	return idx, nil
}
