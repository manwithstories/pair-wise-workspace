package verify

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"

	"segctl/internal/ingest"
	"segctl/internal/m3u8"
	"segctl/internal/report"
)

// Options controls one verification run.
type Options struct {
	MasterPath  string
	Root        string
	ThresholdMs int64   // max |duration deviation| vs the base rendition
	BitrateTol  float64 // allowed relative bitrate deviation
}

// Run verifies every rendition declared by the master playlist against the
// segments on disk and returns the aggregated report.
func Run(opts Options) (*report.Report, error) {
	if opts.ThresholdMs <= 0 {
		opts.ThresholdMs = 200
	}
	if opts.BitrateTol <= 0 {
		opts.BitrateTol = 0.15
	}
	data, err := os.ReadFile(opts.MasterPath)
	if err != nil {
		return nil, fmt.Errorf("verify: read master: %w", err)
	}
	master, err := m3u8.ParseMaster(string(data))
	if err != nil {
		return nil, err
	}
	for _, v := range master.Variants {
		if err := CheckRendition(v.Name); err != nil {
			return nil, err
		}
		if err := m3u8.CheckURI(v.URI); err != nil {
			return nil, err
		}
	}

	type outcome struct {
		row   report.Rendition
		fails []report.Failure
		gaps  int
	}
	results := make([]outcome, len(master.Variants))
	var g errgroup.Group
	for i, v := range master.Variants {
		g.Go(func() error {
			o, err := verifyVariant(opts, v)
			if err != nil {
				return err
			}
			results[i] = o
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Base rendition: the highest-bandwidth variant.
	baseIdx := 0
	for i, v := range master.Variants {
		if v.Bandwidth > master.Variants[baseIdx].Bandwidth {
			baseIdx = i
		}
	}
	baseDur := results[baseIdx].row.DurationMs

	rep := &report.Report{}
	for i, o := range results {
		row := o.row
		row.DeviationMs = row.DurationMs - baseDur
		if abs64(row.DeviationMs) > opts.ThresholdMs {
			o.fails = append(o.fails, report.Failure{
				Category:  "duration_deviation",
				Rendition: row.Rendition,
				Sequence:  -1,
				Detail:    fmt.Sprintf("deviation_ms=%d threshold_ms=%d", row.DeviationMs, opts.ThresholdMs),
			})
		}
		rep.Renditions = append(rep.Renditions, row)
		rep.Failures = append(rep.Failures, o.fails...)
		rep.AddGaps(o.gaps)
		_ = i
	}
	rep.Finalize()
	return rep, nil
}

// CheckRendition rejects rendition names that could escape the root.
func CheckRendition(name string) error {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("verify: unsafe rendition name %q", name)
	}
	return nil
}

func verifyVariant(opts Options, v m3u8.Variant) (struct {
	row   report.Rendition
	fails []report.Failure
	gaps  int
}, error,
) {
	var out struct {
		row   report.Rendition
		fails []report.Failure
		gaps  int
	}
	rend := v.Name
	dir := filepath.Join(opts.Root, rend)
	plPath := filepath.Join(dir, "index.m3u8")
	data, err := os.ReadFile(plPath)
	if err != nil {
		return out, fmt.Errorf("verify: %s: %w", rend, err)
	}
	media, err := m3u8.ParseMedia(string(data))
	if err != nil {
		return out, fmt.Errorf("verify: %s: %w", rend, err)
	}
	if err := media.Validate(); err != nil {
		return out, fmt.Errorf("verify: %s: %w", rend, err)
	}

	// Disk inventory.
	disk := map[string]int64{}
	entries, err := filepath.Glob(filepath.Join(dir, "seg_*.ts"))
	if err != nil {
		return out, err
	}
	for _, e := range entries {
		st, err := os.Stat(e)
		if err != nil {
			return out, err
		}
		disk[filepath.Base(e)] = st.Size()
	}

	fail := func(cat string, seq int64, detail string) {
		out.fails = append(out.fails, report.Failure{
			Category: cat, Rendition: rend, Sequence: seq, Detail: detail,
		})
	}

	declared := map[string]bool{}
	var totalMs, totalBytes int64
	seq := media.MediaSequence
	for _, s := range media.Segments {
		if s.Gap {
			out.gaps++
			seq++
			continue
		}
		declared[s.URI] = true
		size, ok := disk[s.URI]
		if !ok {
			fail("missing_segment", seq, s.URI)
			seq++
			continue
		}
		totalBytes += size
		totalMs += int64(s.DurationMs)
		if s.HasCRC {
			crc, err := ingest.CRC32File(filepath.Join(dir, s.URI))
			if err != nil {
				return out, err
			}
			if crc != s.CRC {
				fail("crc_mismatch", seq, fmt.Sprintf("declared=%08x actual=%08x", s.CRC, crc))
			}
		}
		seq++
	}
	for name := range disk {
		if !declared[name] {
			fail("undeclared_segment", -1, name)
		}
	}

	// Ingest-stage failures recorded by the slicer.
	if meta, err := ingest.ReadMeta(filepath.Join(dir, "meta.json")); err == nil {
		for _, c := range meta.Conflicts {
			fail("crc_conflict", c, "same sequence, different CRC; first arrival kept")
		}
		if meta.Expired > 0 {
			fail("expired", -1, fmt.Sprintf("count=%d", meta.Expired))
		}
	}

	bitrateKbps := 0
	if totalMs > 0 {
		bitrateKbps = int(totalBytes * 8 / totalMs) // bytes*8/ms == kbit/s
	}
	targetKbps := v.Bandwidth / 1000
	if targetKbps > 0 {
		lo := float64(targetKbps) * (1 - opts.BitrateTol)
		hi := float64(targetKbps) * (1 + opts.BitrateTol)
		if float64(bitrateKbps) < lo || float64(bitrateKbps) > hi {
			fail("bitrate_out_of_range", -1,
				fmt.Sprintf("actual=%d target=%d tol=%.2f", bitrateKbps, targetKbps, opts.BitrateTol))
		}
	}

	out.row = report.Rendition{
		Rendition:   rend,
		Segments:    len(declared),
		BitrateKbps: bitrateKbps,
		DurationMs:  totalMs,
	}
	return out, nil
}

// SortFailures stabilizes failure ordering for deterministic reports.
func SortFailures(fs []report.Failure) {
	sort.Slice(fs, func(i, j int) bool {
		if fs[i].Rendition != fs[j].Rendition {
			return fs[i].Rendition < fs[j].Rendition
		}
		if fs[i].Category != fs[j].Category {
			return fs[i].Category < fs[j].Category
		}
		return fs[i].Sequence < fs[j].Sequence
	})
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
