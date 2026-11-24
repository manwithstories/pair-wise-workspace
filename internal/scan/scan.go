// Package scan turns a manifest plus media bytes into a timeline index. It
// never fabricates random access points: only sample numbers declared by stss
// (or every sample when stss is absent) become keyframes. A truncated or
// out-of-range sample is reported with its exact byte offset and the index
// keeps every previously valid entry.
package scan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"mediaroute/internal/clock"
	merr "mediaroute/internal/errors"
	"mediaroute/internal/index"
	"mediaroute/internal/manifest"
)

// Version identifies scanner output. Cached indexes from other versions are
// rejected in favour of a fresh scan.
const Version = "scan-v1"

// Limits guarding resource use on adversarial input.
const (
	// MaxSamplesPerTrack caps the sample table size.
	MaxSamplesPerTrack = 500000
	// MaxKeyframesPerTrack caps the number of accessible keyframes.
	MaxKeyframesPerTrack = 200000
	// MaxReadSampleBytes caps a single sample payload that may be read.
	MaxReadSampleBytes = 8 << 20
)

// Options controls scanning.
type Options struct {
	// BaseDir resolves relative manifest source paths.
	BaseDir string
	// Cache is optional; when set, cached indexes are reused.
	Cache *index.Store
	// VerifyBytes read-ahead validates each sample's byte range. Disabling it
	// keeps scanning metadata-only but loses truncation detection.
	VerifyBytes bool
}

// Result is the outcome of a scan.
type Result struct {
	Index *index.Index
	Key   index.Key
	// CacheHit is true when the index came from the cache unchanged.
	CacheHit bool
	// Rebuilt is true when a cache entry existed but was rejected (corrupt or
	// stale) and the index had to be regenerated.
	Rebuilt bool
}

// DigestFiles hashes the bytes of every manifest source in track order, so the
// cache key changes whenever any media byte changes.
func DigestFiles(baseDir string, m *manifest.Manifest) (string, error) {
	h := sha256.New()
	for _, t := range m.Tracks {
		path := t.Source
		if !filepath.IsAbs(path) {
			path = filepath.Join(baseDir, path)
		}
		f, err := os.Open(path)
		if err != nil {
			return "", merr.New(merr.KindScan, t.ID, -1, -1, false, "open source %s: %v", t.Source, err)
		}
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil {
			return "", merr.New(merr.KindScan, t.ID, -1, -1, false, "read source %s: %v", t.Source, copyErr)
		}
		if closeErr != nil {
			return "", merr.New(merr.KindScan, t.ID, -1, -1, false, "close source %s: %v", t.Source, closeErr)
		}
		// Separate entries so concatenated files cannot collide.
		fmt.Fprintf(h, "\x00track=%d\x00", t.ID)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Run scans all manifest tracks, consulting and populating the cache.
func Run(ctx context.Context, m *manifest.Manifest, opts Options) (*Result, error) {
	if m == nil {
		return nil, merr.New(merr.KindConfig, 0, -1, -1, false, "nil manifest")
	}
	inputDigest, err := DigestFiles(opts.BaseDir, m)
	if err != nil {
		return nil, err
	}
	key := index.Key{InputDigest: inputDigest, ScannerVersion: Version, ManifestDigest: m.Digest}
	res := &Result{Key: key}
	if opts.Cache != nil {
		cached, hit, cerr := opts.Cache.Load(key)
		if cerr != nil {
			res.Rebuilt = true
		}
		if hit && cached != nil {
			res.Index = cached
			res.CacheHit = true
			return res, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, canceled(err)
	}
	ix, scanErr := scanAll(ctx, m, opts)
	if ix == nil {
		return nil, scanErr
	}
	res.Index = ix
	if opts.Cache != nil {
		if err := opts.Cache.Save(key, ix); err != nil {
			// Cache failures are reported as warnings; the scan itself is valid.
			ix.Warnings = append(ix.Warnings, "cache save failed: "+err.Error())
		}
	}
	if scanErr != nil {
		return res, scanErr
	}
	return res, nil
}

func scanAll(ctx context.Context, m *manifest.Manifest, opts Options) (*index.Index, error) {
	ix := &index.Index{
		SchemaVersion:  index.SchemaVersion,
		ScannerVersion: Version,
		ManifestDigest: m.Digest,
		InputDigest:    digestPlaceholder,
	}
	// Recompute key material for embedding in the index.
	inDigest, err := DigestFiles(opts.BaseDir, m)
	if err != nil {
		return nil, err
	}
	ix.InputDigest = inDigest

	var firstErr error
	for _, t := range m.Tracks {
		if err := ctx.Err(); err != nil {
			return nil, canceled(err)
		}
		path := t.Source
		if !filepath.IsAbs(path) {
			path = filepath.Join(opts.BaseDir, path)
		}
		track, ranges, err := scanTrack(ctx, t, path, opts)
		if track != nil {
			ix.Tracks = append(ix.Tracks, *track)
			ix.CorruptRanges = append(ix.CorruptRanges, ranges...)
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	sort.Slice(ix.CorruptRanges, func(i, j int) bool {
		if ix.CorruptRanges[i].TrackID != ix.CorruptRanges[j].TrackID {
			return ix.CorruptRanges[i].TrackID < ix.CorruptRanges[j].TrackID
		}
		return ix.CorruptRanges[i].FirstSample < ix.CorruptRanges[j].FirstSample
	})
	if err := ix.Finalize(); err != nil {
		return nil, merr.New(merr.KindInternal, 0, -1, -1, false, "finalize index: %v", err)
	}
	return ix, firstErr
}

func canceled(err error) error {
	return merr.New(merr.KindCanceled, 0, -1, -1, false, "scan canceled: %v", err)
}

// trackSpan computes a corrupt range's start/end on the shared timeline.
func trackSpan(tb clock.Timebase, editOffset int64, firstDTS, startTicks, endTicks int64) (clock.Rational, clock.Rational, error) {
	start, err := tb.At(firstDTS - editOffset + startTicks)
	if err != nil {
		return clock.Rational{}, clock.Rational{}, err
	}
	end, err := tb.At(firstDTS - editOffset + endTicks)
	if err != nil {
		return clock.Rational{}, clock.Rational{}, err
	}
	return start, end, nil
}

var digestPlaceholder = hex.EncodeToString(make([]byte, sha256.Size))
