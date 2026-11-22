// Package pipeline runs the incremental archive job: it parses the
// manifest, verifies or recomputes each segment, commits an atomic
// checkpoint per segment, and stops at the first unrecoverable input.
// Re-running after an interruption only redoes uncommitted segments.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"archive/internal/cache"
	"archive/internal/checkpoint"
	"archive/internal/clock"
	"archive/internal/demux"
	"archive/internal/fault"
	"archive/internal/manifest"
	"archive/internal/remux"
	"archive/internal/track"
)

// Exit statuses shared with the command adapter.
const (
	ExitOK        = 0
	ExitFailure   = 1
	ExitRecovered = 2 // input damaged, confirmed segments kept
)

// Config controls one run.
type Config struct {
	Root           string
	Manifest       string // manifest path relative to Root, slash separated
	InterruptAfter int    // stop after N newly committed segments; <=0 disables
	Verbose        bool
}

// Result reports what happened during the run.
type Result struct {
	ManifestSeen int // segment references in the manifest
	Unique       int // distinct segment URIs
	Committed    int // newly processed and checkpointed this run
	Verified     int // reused from a verified checkpoint
	Duplicates   int // duplicate manifest references dropped
	Pending      int // unique segments still uncommitted
	FirstBad     string
	FirstBadIdx  int
	Entries      []checkpoint.Entry
	Latencies    []time.Duration
	Exit         int
}

// Run executes the pipeline.
func Run(cfg Config) (Result, error) {
	res := Result{FirstBadIdx: -1}
	if cfg.Root == "" {
		return res, fault.Newf(fault.Fatal, "pipeline", "", "empty root")
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return res, fault.Wrap(fault.Fatal, "pipeline", cfg.Root, err)
	}
	manifestPath, err := safeSegmentPath(root, cfg.Manifest)
	if err != nil {
		return res, err
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return res, fault.Wrap(fault.Fatal, "pipeline", cfg.Manifest, err)
	}
	base := path.Dir(cfg.Manifest)
	if base == "." {
		base = ""
	}
	segs, err := manifest.ParseHLS(raw, cfg.Manifest, base)
	if err != nil {
		return res, err
	}
	res.ManifestSeen = len(segs)

	cas, err := cache.Open(filepath.Join(root, "work", "cas"))
	if err != nil {
		return res, err
	}
	cp, err := checkpoint.Open(filepath.Join(root, "work", "checkpoints"))
	if err != nil {
		return res, err
	}

	// Duplicate references are a recoverable anomaly: the same content
	// may legitimately appear twice with the same presentation start,
	// but a repeat with a conflicting start is corruption. Either way
	// the segment is processed once; the earliest reference defines its
	// timeline so output does not depend on manifest order.
	byURI := map[string]int{}
	var unique []manifest.Segment
	for _, s := range segs {
		if idx, dup := byURI[s.URI]; dup {
			res.Duplicates++
			if unique[idx].Start.Cmp(s.Start) != 0 {
				return res, fault.Newf(fault.Fatal, "pipeline", s.URI,
					"duplicate reference has a conflicting timeline")
			}
			continue
		}
		byURI[s.URI] = len(unique)
		unique = append(unique, s)
	}
	res.Unique = len(unique)

	var signature string
	for _, seg := range unique {
		// InterruptAfter: positive N stops after N newly committed
		// segments (simulating a process exit); zero or negative runs to
		// completion.
		if cfg.InterruptAfter > 0 && res.Committed >= cfg.InterruptAfter {
			break // simulated process exit after an atomic checkpoint
		}
		start := time.Now()
		entry, status, err := processOne(root, cas, cp, seg, &signature)
		res.Latencies = append(res.Latencies, time.Since(start))
		// A fault means nothing from this segment counts: a Degraded
		// segment has already had its checkpoint withdrawn, and a Fatal
		// one never committed anything usable.
		if err == nil {
			switch status {
			case statusVerified:
				res.Verified++
				res.Entries = append(res.Entries, entry)
			case statusCommitted:
				res.Committed++
				res.Entries = append(res.Entries, entry)
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAULT segment=%s err=%v level=%s\n", seg.URI, err, fault.LevelOf(err))
			if fault.LevelOf(err) == fault.Fatal {
				res.Exit = ExitFailure
				return res, err
			}
			res.FirstBad = seg.URI
			res.FirstBadIdx = seg.Index
			res.Exit = ExitRecovered
			res.Pending = res.Unique - len(res.Entries)
			return res, nil
		}
		if cfg.Verbose {
			fmt.Printf("segment=%s status=%s tracks=%d bytes=%d\n",
				seg.URI, status, entry.Tracks, entry.OutputBytes)
		}
	}
	res.Pending = res.Unique - len(res.Entries)
	res.Exit = ExitOK
	return res, nil
}

type status int

const (
	statusVerified status = iota
	statusCommitted
)

func (s status) String() string {
	if s == statusVerified {
		return "verified"
	}
	return "committed"
}

// processOne verifies or (re)builds a single segment. A Degraded fault
// is returned with its level intact so the caller can stop at the first
// bad dependency while keeping everything already committed.
func processOne(root string, cas *cache.Store, cp *checkpoint.Store, seg manifest.Segment, signature *string) (checkpoint.Entry, status, error) {
	rel, err := safeSegmentPath(root, seg.URI)
	if err != nil {
		return checkpoint.Entry{}, statusCommitted, err
	}
	data, err := readCapped(rel)
	if err != nil {
		return checkpoint.Entry{}, statusCommitted, err
	}
	inputSHA := cache.Digest(data)

	if prev, ok, err := cp.Load(seg.Index); err != nil {
		return checkpoint.Entry{}, statusCommitted, err
	} else if ok {
		if prev.InputSHA == inputSHA && prev.MappingVersion == track.Version &&
			cas.Verified(prev.OutputSHA, seg.URI) {
			return prev, statusVerified, nil
		}
		// Stale checkpoint: the input or cache changed. Fall through and
		// rebuild; the CAS quarantines any corrupt hit it finds.
	}

	tracks, derr := demux.Demux(data, seg.URI)
	if derr == nil && len(tracks) == 0 {
		return checkpoint.Entry{}, statusCommitted,
			fault.Newf(fault.Degraded, "pipeline", seg.URI, "no tracks")
	}
	if derr != nil {
		// Truncated or otherwise damaged: even a parseable prefix is not
		// a confirmed segment. Stop here without writing CAS output,
		// provenance or a checkpoint, so nothing partial can be mistaken
		// for a committed result. Earlier segments stay committed.
		return checkpoint.Entry{}, statusCommitted, derr
	}

	order, err := track.Map(tracks)
	if err != nil {
		return checkpoint.Entry{}, statusCommitted, err
	}
	sig := signatureOf(tracks)
	if *signature == "" {
		*signature = sig
	} else if *signature != sig {
		return checkpoint.Entry{}, statusCommitted, fault.Newf(fault.Fatal, "pipeline", seg.URI,
			"track layout changed: %q then %q", *signature, sig)
	}

	ordered := make([]demux.Track, len(tracks))
	for i, pos := range order {
		ordered[pos] = tracks[i]
	}
	inputs := make([]remux.Input, len(ordered))
	for i, tr := range ordered {
		ds, err := timeline(seg, tr)
		if err != nil {
			return checkpoint.Entry{}, statusCommitted, err
		}
		inputs[i] = remux.Input{Track: tr, DTS: ds}
	}
	out, info, err := remux.Write(inputs)
	if err != nil {
		return checkpoint.Entry{}, statusCommitted, err
	}
	obj, _, err := cas.Put(seg.URI, out)
	if err != nil {
		return checkpoint.Entry{}, statusCommitted, err
	}
	entry := checkpoint.Entry{
		Index:          seg.Index,
		URI:            seg.URI,
		InputSHA:       inputSHA,
		MappingVersion: track.Version,
		OutputSHA:      obj.Digest,
		OutputBytes:    obj.Bytes,
		Tracks:         len(info.Tracks),
	}
	prov := Provenance{
		Index: seg.Index, Segment: seg.URI, ManifestSource: seg.Source,
		InputSHA256: inputSHA, OutputSHA256: obj.Digest,
		MappingVersion: track.Version, Tracks: info.Tracks,
		SyncMs: info.SyncMs, Bytes: obj.Bytes,
	}
	if err := writeProvenance(root, prov); err != nil {
		return checkpoint.Entry{}, statusCommitted, err
	}
	if err := cp.Commit(entry); err != nil {
		return checkpoint.Entry{}, statusCommitted, err
	}
	return entry, statusCommitted, nil
}

// timeline places a track's samples on the manifest presentation
// timeline. The manifest anchor says which presentation time a stored
// counter of zero means; each raw counter is mapped relative to that
// modulo the source wrap period. A counter that restarted at a wrap
// therefore lands on the true timeline, and relative offsets between
// tracks (capture skew) survive for the metrics module to measure.
// No cross-segment state is involved, which is what makes a resumed
// run reproduce identical bytes.
func timeline(seg manifest.Segment, tr demux.Track) ([]int64, error) {
	if len(tr.Samples) == 0 {
		return nil, fault.Newf(fault.Degraded, "pipeline", seg.URI, "track without samples")
	}
	base, err := seg.EffectiveAnchor().Scale(int64(tr.Timescale))
	if err != nil {
		return nil, fault.Wrap(fault.Fatal, "pipeline", seg.URI, err)
	}
	period := clock.WrapPeriod(tr.Timescale)
	ds := make([]int64, len(tr.Samples))
	for j, s := range tr.Samples {
		d := clock.SignedDelta(s.DTS, 0, period)
		// Guard the addition without overflowing the bounds themselves:
		// d is small, so branch on its sign.
		if (d > 0 && base > math.MaxInt64-d) || (d < 0 && base < math.MinInt64-d) {
			return nil, fault.Newf(fault.Fatal, "pipeline", seg.URI, "decode time overflow")
		}
		ds[j] = base + d
	}
	return ds, nil
}

func signatureOf(tracks []demux.Track) string {
	parts := make([]string, 0, len(tracks))
	for _, t := range tracks {
		parts = append(parts, track.Role(t)+"|"+t.Lang+"|"+t.Codec)
	}
	// Deterministic order independent of demux order.
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && parts[j] < parts[j-1]; j-- {
			parts[j], parts[j-1] = parts[j-1], parts[j]
		}
	}
	return strings.Join(parts, ",")
}

// safeSegmentPath joins a manifest-relative URI to root and rejects any
// path that escapes root after cleaning.
func safeSegmentPath(root, rel string) (string, error) {
	if strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") {
		return "", fault.Newf(fault.Fatal, "pipeline", rel, "unsafe segment path")
	}
	joined := filepath.Join(root, filepath.FromSlash(rel))
	cleanRoot := filepath.Clean(root) + string(filepath.Separator)
	if !strings.HasPrefix(joined+string(filepath.Separator), cleanRoot) {
		return "", fault.Newf(fault.Fatal, "pipeline", rel, "segment escapes root")
	}
	return joined, nil
}

// readCapped reads at most manifest.MaxSegmentBytes; anything larger is
// rejected before it can exhaust memory.
func readCapped(name string) ([]byte, error) {
	f, err := os.Open(name)
	if os.IsNotExist(err) {
		return nil, fault.Newf(fault.Degraded, "pipeline", filepath.Base(name), "segment missing")
	}
	if err != nil {
		return nil, fault.Wrap(fault.Fatal, "pipeline", name, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, manifest.MaxSegmentBytes+1))
	if err != nil {
		return nil, fault.Wrap(fault.Fatal, "pipeline", name, err)
	}
	if len(data) > manifest.MaxSegmentBytes {
		return nil, fault.Newf(fault.Fatal, "pipeline", name, "segment exceeds 64 MiB")
	}
	return data, nil
}

// DigestEntries returns a deterministic digest over the committed set,
// independent of processing order or run history.
func DigestEntries(entries []checkpoint.Entry) string {
	sorted := append([]checkpoint.Entry(nil), entries...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].Index < sorted[j-1].Index; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	h := sha256.New()
	for _, e := range sorted {
		fmt.Fprintf(h, "%d\t%s\t%s\t%s\n", e.Index, e.URI, e.InputSHA, e.OutputSHA)
	}
	return hex.EncodeToString(h.Sum(nil))
}
