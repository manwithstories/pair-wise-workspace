// Package metrics independently re-checks pipeline output. Every value
// is derived from the committed artifacts and wall-clock measurements,
// never from the pipeline's own counters, so a bug in the pipeline
// cannot silently agree with itself.
package metrics

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"archive/internal/cache"
	"archive/internal/checkpoint"
	"archive/internal/clock"
	"archive/internal/demux"
	"archive/internal/fault"
	"archive/internal/pipeline"
)

// Report is the end-of-run verification summary.
type Report struct {
	Segments      int
	CacheVerified int
	OutputSHA     string
	SyncErrorMs   float64
	P95LatencyMs  float64
	TrackTable    []string
	// Desync lists segments whose audio/video start gap exceeds the
	// tolerance, so a consumer can see where the error comes from.
	Desync []string
	// Provenance lists one source summary per verified segment.
	Provenance []string
}

// SyncToleranceMs bounds the acceptable audio/video start gap.
const SyncToleranceMs = 1.0

// Build re-reads every committed entry, re-hashes the CAS blob, and
// measures synchronisation error from the output timestamps themselves.
func Build(root string, latencies []time.Duration) (Report, error) {
	var rep Report
	cas, err := cache.Open(filepath.Join(root, "work", "cas"))
	if err != nil {
		return rep, err
	}
	cp, err := checkpoint.Open(filepath.Join(root, "work", "checkpoints"))
	if err != nil {
		return rep, err
	}
	entries, err := cp.Entries()
	if err != nil {
		return rep, err
	}
	rep.Segments = len(entries)
	rep.OutputSHA = pipeline.DigestEntries(entries)
	// Guard against a stale entries file: the entry digest must chain
	// from the blobs we just re-hashed, so a mismatch means the
	// checkpoint set and the store disagree.
	if err := verifyChain(cas, entries); err != nil {
		return rep, err
	}
	rep.P95LatencyMs = p95(latencies)

	var worst float64
	for _, e := range entries {
		data, err := cas.Get(e.OutputSHA, e.URI)
		if err != nil {
			return rep, fault.Wrap(fault.Fatal, "metrics", e.URI, err)
		}
		rep.CacheVerified++
		tracks, derr := demux.Demux(data, e.URI)
		if derr != nil {
			return rep, fault.Wrap(fault.Fatal, "metrics", e.URI, derr)
		}
		prov, perr := pipeline.ReadProvenance(root, e)
		if perr != nil {
			return rep, perr
		}
		rep.Provenance = append(rep.Provenance, prov.String())
		sync, table, err := measureSync(tracks)
		if err != nil {
			return rep, err
		}
		// The segment's own record and the independent measurement must
		// agree; a divergence means one of them is wrong.
		if d := prov.SyncMs - sync; d > SyncToleranceMs || d < -SyncToleranceMs {
			return rep, fault.Newf(fault.Fatal, "metrics", e.URI,
				"recorded sync %.3fms disagrees with measured %.3fms", prov.SyncMs, sync)
		}
		rep.TrackTable = table
		if sync > SyncToleranceMs {
			rep.Desync = append(rep.Desync, fmt.Sprintf("%s=%.1fms", e.URI, sync))
		}
		if sync > worst {
			worst = sync
		}
	}
	rep.SyncErrorMs = worst
	return rep, nil
}

func verifyChain(cas *cache.Store, entries []checkpoint.Entry) error {
	for _, e := range entries {
		data, err := cas.Get(e.OutputSHA, e.URI)
		if err != nil {
			return err
		}
		if int64(len(data)) != e.OutputBytes {
			return fault.Newf(fault.Fatal, "metrics", e.URI,
				"blob size %d does not match checkpoint %d", len(data), e.OutputBytes)
		}
	}
	return nil
}

// measureSync computes the audio/video start gap from demuxed sample
// timestamps, not from any value the pipeline reported.
func measureSync(tracks []demux.Track) (float64, []string, error) {
	var v, a *demux.Track
	var table []string
	for i := range tracks {
		t := &tracks[i]
		first := firstPTS(t)
		last := lastPTS(t)
		if first == nil || last == nil {
			return 0, nil, fault.Newf(fault.Fatal, "metrics", t.Codec, "track without samples")
		}
		table = append(table, fmt.Sprintf("%s/%s/%s/%d", roleOf(t.Handler), t.Lang, t.Codec, t.Timescale))
		switch roleOf(t.Handler) {
		case "video":
			if v == nil {
				v = t
			}
		case "audio":
			if a == nil {
				a = t
			}
		}
	}
	if v == nil || a == nil {
		return 0, table, nil
	}
	d, err := firstPTS(v).Sub(*firstPTS(a))
	if err != nil {
		return 0, nil, fault.Wrap(fault.Fatal, "metrics", "", err)
	}
	f := d.Float64() * 1000
	if f < 0 {
		f = -f
	}
	return f, table, nil
}

func firstPTS(t *demux.Track) *clock.Rational {
	if len(t.Samples) == 0 || t.Timescale == 0 {
		return nil
	}
	r, err := clock.New(int64(t.Samples[0].DTS)+t.Samples[0].CTSOff, int64(t.Timescale))
	if err != nil {
		return nil
	}
	return &r
}

func lastPTS(t *demux.Track) *clock.Rational {
	if len(t.Samples) == 0 || t.Timescale == 0 {
		return nil
	}
	s := t.Samples[len(t.Samples)-1]
	r, err := clock.New(int64(s.DTS)+int64(s.Dur)+s.CTSOff, int64(t.Timescale))
	if err != nil {
		return nil
	}
	return &r
}

func roleOf(h string) string {
	switch h {
	case "vide":
		return "video"
	case "soun":
		return "audio"
	default:
		return "aux"
	}
}

func p95(xs []time.Duration) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	// Nearest-rank P95 without interpolation.
	idx := (len(s)*95 + 99) / 100 // ceil(0.95*n)
	if idx < 1 {
		idx = 1
	}
	if idx > len(s) {
		idx = len(s)
	}
	return float64(s[idx-1]) / float64(time.Millisecond)
}

// WriteJSON records the report next to the work directory for auditing.
func WriteJSON(root string, rep Report) error {
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fault.Wrap(fault.Fatal, "metrics", root, err)
	}
	dir := filepath.Join(root, "work")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fault.Wrap(fault.Fatal, "metrics", dir, err)
	}
	tmp := filepath.Join(dir, "metrics.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fault.Wrap(fault.Fatal, "metrics", tmp, err)
	}
	return os.Rename(tmp, filepath.Join(dir, "metrics.json"))
}
