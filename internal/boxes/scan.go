package boxes

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/abema/go-mp4"

	"bcast/internal/media"
)

// ScanOptions controls bounded scanning.
type ScanOptions struct {
	Concurrency int
}

// ScanBatch scans init.mp4 and every *.m4s segment in dir.  A non-nil report
// is returned even when a fragment is corrupt; the returned error carries the
// first deterministic located failure.
func ScanBatch(dir string, opts ScanOptions) (*BatchReport, error) {
	rep := &BatchReport{Dir: dir, Init: InitInfo{Timescales: map[uint32]uint32{}}}
	initPath := filepath.Join(dir, "init.mp4")
	init, err := scanInit(initPath)
	if err != nil {
		rep.Fatal = asMediaError(err)
		return rep, err
	}
	rep.Init = *init

	entries, err := os.ReadDir(dir)
	if err != nil {
		e := media.IO("scan", dir, 0, 0, err)
		rep.Fatal = e
		return rep, e
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if strings.HasSuffix(strings.ToLower(name), ".m4s") {
			rep.Files = append(rep.Files, filepath.Join(dir, name))
		}
	}
	sort.Strings(rep.Files)
	if len(rep.Files) == 0 {
		e := media.Corrupt("scan", "no_segments", dir, 0, 0, "no .m4s fragments found")
		rep.Fatal = e
		return rep, e
	}

	workers := opts.Concurrency
	if workers <= 0 {
		workers = 4
	}
	if workers > len(rep.Files) {
		workers = len(rep.Files)
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	results := make([]*Fragment, len(rep.Files))
	errs := make([]error, len(rep.Files))
	for i, path := range rep.Files {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, path string) {
			defer wg.Done()
			defer func() { <-sem }()
			frag, err := parseSegment(path, i+1, rep.Init.Timescales)
			results[i] = frag
			errs[i] = err
		}(i, path)
	}
	wg.Wait()

	var firstErr error
	for i := range results {
		if results[i] != nil {
			rep.Fragments = append(rep.Fragments, results[i])
		}
		if errs[i] != nil {
			if firstErr == nil {
				firstErr = errs[i]
			}
			if results[i] != nil {
				results[i].Isolated = true
				if me, ok := media.AsError(errs[i]); ok {
					results[i].Error = me
				}
				rep.Isolated = append(rep.Isolated, results[i])
			}
		}
	}
	if firstErr != nil {
		me := asMediaError(firstErr)
		rep.Fatal = me
		return rep, me
	}

	rep.Sequences = AnalyzeSequences(rep.Fragments)
	rep.Unique = UniqueFragments(rep.Fragments, rep.Sequences)
	if len(rep.Unique) == 0 {
		e := media.Corrupt("scan", "all_fragments_duplicate", dir, 0, 0, "content digest dedup removed every fragment")
		rep.Fatal = e
		return rep, e
	}
	return rep, nil
}

func parseSegment(path string, ordinal int, timescales map[uint32]uint32) (*Fragment, error) {
	frag := &Fragment{No: fragmentNoFromName(path, ordinal), Path: path}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return frag, media.Corrupt("scan", "segment_missing", path, uint32(frag.No), 0, "%v", err)
		}
		return frag, media.IO("scan", path, uint32(frag.No), 0, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return frag, media.IO("scan", path, uint32(frag.No), 0, err)
	}
	fileSize := st.Size()

	for pos := int64(0); pos < fileSize; {
		if _, err := f.Seek(pos, io.SeekStart); err != nil {
			return frag, media.IO("scan", path, uint32(frag.No), pos, err)
		}
		bi, err := mp4.ReadBoxInfo(f)
		if err != nil {
			off := fileSize
			if pos < fileSize && fileSize-pos < 8 {
				off = fileSize // first unavailable byte of truncated header
			}
			return frag, media.Corrupt("scan", "short_box_header", path, uint32(frag.No), off, "%v", err)
		}
		start := int64(bi.Offset)
		headerSize := int64(bi.HeaderSize)
		if bi.Size > uint64(fileSize-start) {
			return frag, media.Corrupt("scan", "box_length_out_of_bounds", path, uint32(frag.No), fileSize,
				"box %s at %d declares %d bytes beyond EOF %d", boxType(bi.Type), start, bi.Size, fileSize)
		}
		size := int64(bi.Size)
		end, err := safeEnd(start, size)
		if err != nil || size < headerSize || end > fileSize {
			off := start
			if err != nil {
				off = start + headerSize
			}
			return frag, media.Corrupt("scan", "invalid_box_length", path, uint32(frag.No), off,
				"box %s has invalid size %d (header=%d)", boxType(bi.Type), bi.Size, bi.HeaderSize)
		}

		switch boxType(bi.Type) {
		case "moof":
			if err := parseMoof(f, bi, frag, timescales); err != nil {
				return frag, mapScanError(err, path, frag.Sequence)
			}
		case "mdat":
			if err := hashMDAT(f, bi, frag); err != nil {
				return frag, mapScanError(err, path, frag.Sequence)
			}
		}
		pos = end
	}
	if err := validateFragment(frag, timescales, fileSize); err != nil {
		return frag, err
	}
	return frag, nil
}

func hashMDAT(f *os.File, bi *mp4.BoxInfo, frag *Fragment) error {
	start := int64(bi.Offset)
	headerSize := int64(bi.HeaderSize)
	payloadStart := start + headerSize
	payloadEnd := start + int64(bi.Size)
	frag.DataStart = payloadStart
	frag.DataEnd = payloadEnd
	frag.PayloadBytes = payloadEnd - payloadStart
	h := sha256.New()
	if _, err := bi.SeekToPayload(f); err != nil {
		return &boxScanError{offset: payloadStart, kind: "mdat_seek_failed", err: err}
	}
	if _, err := io.CopyN(h, f, frag.PayloadBytes); err != nil {
		return &boxScanError{offset: payloadStart, kind: "mdat_hash_failed", err: err}
	}
	frag.ContentSHA256 = hex.EncodeToString(h.Sum(nil))
	return nil
}

func asMediaError(err error) *media.Error {
	if me, ok := media.AsError(err); ok {
		return me
	}
	return media.Corrupt("scan", "scan_failed", "", 0, -1, "%v", err)
}
