package audit

import (
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"archivectl/internal/clock"
	"archivectl/internal/index"
	"archivectl/internal/layout"
	"archivectl/internal/scan"
)

var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func shaRE(s string) bool { return shaPattern.MatchString(s) }

func percentile95(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	sort.Float64s(samples)
	idx := int(float64(len(samples)-1) * 0.95)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(samples) {
		idx = len(samples) - 1
	}
	return samples[idx]
}

type boundaryRef struct {
	chunk    int
	boundary int
}

// randomSeekChecks is the deterministic number of random seek points.
const randomSeekChecks = 24

// verifyChunks re-scans every repaired chunk for timing p95 and AV sync,
// then checks deterministic random seek points against both the index
// boundary and the original input payload bytes.
func verifyChunks(root, group string, idx *index.Index) (float64, float64, int, bool, error) {
	timings := make([]float64, 0, len(idx.Chunks))
	type startKey struct{ track string }
	starts := map[string]map[int]clock.Rational{}
	frags := map[int]*scan.Fragment{}

	for ci := range idx.Chunks {
		c := &idx.Chunks[ci]
		outPath := filepath.Join(layout.OutDir(root, group), filepath.FromSlash(c.OutputRel))
		t0 := time.Now()
		rep, err := scan.ScanFile(outPath, c.TrackID)
		elapsed := float64(time.Since(t0).Nanoseconds()) / 1e6
		timings = append(timings, elapsed)
		if err != nil {
			return 0, 0, 0, false, err
		}
		if rep.Record.HasFatal() || len(rep.Fragments) == 0 {
			return 0, 0, 0, false, nil
		}
		f := rep.Fragments[0]
		frags[ci] = f
		if _, ok := starts[c.TrackID]; !ok {
			starts[c.TrackID] = map[int]clock.Rational{}
		}
		starts[c.TrackID][c.SegmentIndex] = clock.New(int64(f.TFDT.Base), int64(c.Timescale))
	}

	avWorst := 0.0
	if v, a := starts["video"], starts["audio"]; v != nil && a != nil {
		for seg, vr := range v {
			if ar, ok := a[seg]; ok {
				if d := clock.MillisDiff(vr, ar); d > avWorst {
					avWorst = d
				}
			}
		}
	}
	p95 := percentile95(timings)

	// Build the flat boundary list and choose deterministic random points.
	var refs []boundaryRef
	for ci := range idx.Chunks {
		for bi := range idx.Chunks[ci].Samples {
			refs = append(refs, boundaryRef{ci, bi})
		}
	}
	rng := rand.New(rand.NewSource(41))
	pickCount := randomSeekChecks
	if pickCount > len(refs) {
		pickCount = len(refs)
	}
	order := rng.Perm(len(refs))
	payloadOK := true
	checks := 0
	for k := 0; k < pickCount; k++ {
		ref := refs[order[k]]
		c := &idx.Chunks[ref.chunk]
		b := &c.Samples[ref.boundary]
		sf := frags[ref.chunk]
		if sf == nil || b.SampleIndex >= len(sf.Samples) {
			payloadOK = false
			continue
		}
		ss := sf.Samples[b.SampleIndex]
		commonDTS, exact := clock.RescaleExact(ss.DTS, c.Timescale, clock.CommonTimescale)
		if !exact || commonDTS != b.DTSCommon || ss.Size != b.Size {
			payloadOK = false
		}
		if !bytesMatchAt(root, group, c, b) {
			payloadOK = false
		}
		checks++
	}
	return avWorst, p95, checks, payloadOK, nil
}

// bytesMatchAt compares the sample bytes at the indexed absolute offset
// between the original input chunk and the repaired output chunk.
func bytesMatchAt(root, group string, c *index.Chunk, b *index.SampleBoundary) bool {
	srcPath := filepath.Join(layout.InDir(root, group), filepath.FromSlash(c.SourceRel))
	outPath := filepath.Join(layout.OutDir(root, group), filepath.FromSlash(c.OutputRel))
	src, err := os.ReadFile(srcPath)
	if err != nil {
		return false
	}
	out, err := os.ReadFile(outPath)
	if err != nil {
		return false
	}
	start := b.ByteOffset
	n := int64(b.Size)
	if start < 0 || start+n > int64(len(src)) || start+n > int64(len(out)) {
		return false
	}
	for i := int64(0); i < n; i++ {
		if src[start+i] != out[start+i] {
			return false
		}
	}
	return true
}

// probeDigestInvalidation copies the input tree to a temp directory,
// mutates it and confirms the whole-tree digest changes.
func probeDigestInvalidation(root, group, base string) (bool, error) {
	src := layout.InDir(root, group)
	tmp, err := os.MkdirTemp("", "audit-inv-*")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp)
	if err := copyTree(src, tmp); err != nil {
		return false, err
	}
	probe := filepath.Join(tmp, "mutation-probe.bin")
	if err := os.WriteFile(probe, []byte("the smallest change"), 0o644); err != nil {
		return false, err
	}
	d, err := index.DigestTree(tmp)
	if err != nil {
		return false, err
	}
	return d != base, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		buf := make([]byte, 32*1024)
		_, err = io.CopyBuffer(out, in, buf)
		return err
	})
}
