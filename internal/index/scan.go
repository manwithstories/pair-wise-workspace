package index

import (
	"errors"
	"fmt"
	"io"
	"os"

	"mediascan/internal/manifest"
	"mediascan/internal/model"
)

// Scan errors.
var (
	ErrMissingMedia = errors.New("index: referenced media is missing")
	ErrBadRange     = errors.New("index: byte range outside file")
	ErrTooLarge     = errors.New("index: media file too large")
)

// MaxSegmentBytes caps each referenced media file at 16 MiB.
const MaxSegmentBytes = 16 << 20

// Options controls Scan.
type Options struct {
	// ResumeFrom is the number of leading entries already hashed by a prior
	// interrupted run. Entries before it are copied from Prev without
	// re-reading the media, but only when their digests still match.
	ResumeFrom int
	Prev       *Index
}

// Scan walks the timeline in manifest order, hashing each present segment in
// a streaming pass. Missing media leaves a gap in the timeline and records the
// first missing dependency; scanning continues so a partial index is always
// produced.
func Scan(res *manifest.Resolver, tl *manifest.Timeline, opts Options) (*Index, error) {
	idx := &Index{
		Version:   Version,
		Manifest:  tl.Source,
		Format:    tl.Format,
		Timescale: tl.Timescale,
		Duration:  tl.Duration,
		InputSHA:  HashTimeline(tl),
	}
	var faults model.Faults
	ordinal := 0
	for _, stream := range tl.Streams {
		for _, seg := range stream.Segments {
			entry := Entry{
				Ordinal:   ordinal,
				StreamID:  stream.ID,
				Name:      seg.Name,
				URI:       seg.URI,
				ByteStart: seg.ByteStart,
				ByteEnd:   seg.ByteEnd,
				Start:     seg.Start,
				Duration:  seg.Duration,
				Timescale: stream.Timescale,
			}
			full, err := res.Resolve(seg.URI)
			if err != nil {
				return nil, err
			}
			resumed := false
			if opts.Prev != nil && ordinal < opts.ResumeFrom && ordinal < len(opts.Prev.Entries) {
				prev := opts.Prev.Entries[ordinal]
				if prev.URI == seg.URI && prev.ByteStart == seg.ByteStart &&
					prev.ByteEnd == seg.ByteEnd && prev.SHA256 != "" {
					entry = prev
					entry.Ordinal = ordinal
					entry.Start, entry.Duration = seg.Start, seg.Duration
					entry.Present = true
					resumed = true
					idx.Skipped++
				}
			}
			if !resumed {
				if err := scanOne(res, full, &entry, ordinal, &faults); err != nil {
					var scanErr *mediaError
					if !errors.As(err, &scanErr) {
						return nil, err
					}
					entry.Present = false
					idx.Missing = append(idx.Missing, seg.URI)
					if idx.FirstMiss == "" {
						idx.FirstMiss = seg.URI
					}
					faults.Add(scanErr.fault)
					idx.Entries = append(idx.Entries, entry)
					ordinal++
					continue
				}
				idx.Scanned++
			}
			idx.Entries = append(idx.Entries, entry)
			ordinal++
		}
	}
	faults.Sort()
	idx.Faults = faults.All()
	idx.Missing = append(idx.Missing, tl.Missing...)
	if idx.FirstMiss == "" {
		idx.FirstMiss = tl.FirstMiss
	}
	return idx, nil
}

// mediaError carries a classified fault for a single segment while letting
// Scan continue with the rest of the timeline.
type mediaError struct {
	fault model.Fault
}

func (e *mediaError) Error() string { return e.fault.Summary() }

func scanOne(res *manifest.Resolver, full string, entry *Entry, ordinal int, faults *model.Faults) error {
	info, err := os.Stat(full)
	if err != nil || !info.Mode().IsRegular() {
		return &mediaError{fault: model.Fault{
			Kind: model.FaultMissingSegment, Severity: model.SeverityCritical,
			Segment: entry.Name, Message: "manifest references absent media",
			TimeStart: entry.Start, IndexEntry: ordinal,
		}}
	}
	size := info.Size()
	if size > MaxSegmentBytes {
		return &mediaError{fault: model.Fault{
			Kind: model.FaultBoxBounds, Severity: model.SeverityCritical,
			Segment: entry.Name, Message: fmt.Sprintf("media exceeds %d bytes", MaxSegmentBytes),
			ByteStart: 0, ByteEnd: size - 1,
			TimeStart: entry.Start, IndexEntry: ordinal,
		}}
	}
	if entry.ByteEnd > 0 {
		if entry.ByteEnd >= size || entry.ByteStart >= size {
			return &mediaError{fault: model.Fault{
				Kind: model.FaultBoxBounds, Severity: model.SeverityCritical,
				Segment: entry.Name, Message: "manifest byte range exceeds media size",
				ByteStart: entry.ByteStart, ByteEnd: entry.ByteEnd,
				TimeStart: entry.Start, IndexEntry: ordinal,
			}}
		}
	}
	digest, err := hashRange(full, entry.ByteStart, entry.ByteEnd)
	if err != nil {
		return &mediaError{fault: model.Fault{
			Kind: model.FaultMissingSegment, Severity: model.SeverityCritical,
			Segment: entry.Name, Message: err.Error(),
			ByteStart: entry.ByteStart, ByteEnd: entry.ByteEnd,
			TimeStart: entry.Start, IndexEntry: ordinal,
		}}
	}
	entry.SHA256 = digest
	entry.Size = size
	entry.Present = true
	return nil
}

// hashRange hashes [start,end] when a range is set, else the whole file.
func hashRange(path string, start, end int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if end == -1 {
		if start != 0 {
			return "", fmt.Errorf("%w", ErrBadRange)
		}
		return HashReader(f)
	}
	if end < start {
		return "", fmt.Errorf("%w", ErrBadRange)
	}
	if _, err := f.Seek(start, 0); err != nil {
		return "", err
	}
	return HashReader(io.LimitReader(f, end-start+1))
}
