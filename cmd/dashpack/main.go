// dashpack packs multi-tier synthetic renditions into a DASH manifest and
// verifies manifests against on-disk segments. Pure terminal tool, no
// external services.
//
// Exit codes: 0 ok, 2 parameter error, 3 alignment over tolerance,
// 4 missing segments, 5 manifest parse failure.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"dashpack/internal/align"
	"dashpack/internal/errs"
	"dashpack/internal/ingest"
	"dashpack/internal/mpd"
	"dashpack/internal/pack"
	"dashpack/internal/verify"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return errs.ExitParam
	}
	switch args[0] {
	case "pack":
		return cmdPack(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	default:
		usage()
		return errs.ExitParam
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  dashpack pack   --reps N --duration SEC --seg SEC --seed N --out MPD [--report JSON] [--in DIR] [--addr number|time] [--force] [--tolerance-ms N]
  dashpack verify --mpd MPD [--json] [--tolerance-ms N]
exit codes: 0 ok, 2 param, 3 alignment, 4 missing segments, 5 manifest parse`)
}

func cmdPack(args []string) int {
	fs := flag.NewFlagSet("pack", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	reps := fs.Int64("reps", 4, "number of renditions")
	duration := fs.Int64("duration", 600, "total duration in seconds")
	seg := fs.Int64("seg", 4, "segment duration in seconds")
	seed := fs.Int64("seed", 1, "deterministic seed")
	out := fs.String("out", "", "output manifest path")
	reportPath := fs.String("report", "", "optional JSON report path")
	in := fs.String("in", "", "input segment root (empty: synthesise)")
	addr := fs.String("addr", "number", "addressing mode: number|time")
	force := fs.Bool("force", false, "overwrite differing outputs")
	tol := fs.Int64("tolerance-ms", 10, "alignment tolerance in ms")
	if err := fs.Parse(args); err != nil {
		usage()
		return errs.ExitParam
	}
	if *out == "" || (*addr != pack.AddrNumber && *addr != pack.AddrTime) {
		usage()
		return errs.ExitParam
	}
	tracks, aerr := loadTracks(*in, *seed, *reps, *duration, *seg)
	if aerr != nil {
		return fail(aerr)
	}
	aligned, err := align.Align(tracks, *seg)
	if err != nil {
		return fail(mapAlignErr(err))
	}
	m, err := pack.BuildManifest(aligned, *addr)
	if err != nil {
		return fail(errs.New(errs.ExitParam, errs.CodeParam, err.Error(), nil))
	}
	if err := mpd.CheckRoundTrip(m); err != nil {
		return fail(errs.New(errs.ExitManifest, errs.CodeRoundTrip, err.Error(), nil))
	}
	data, err := mpd.Marshal(m)
	if err != nil {
		return fail(errs.New(errs.ExitManifest, errs.CodeManifestParse, err.Error(), nil))
	}
	// Pre-flight: refuse to clobber a differing manifest before writing
	// hundreds of segment files.
	if _, err := pack.WriteAtomic(*out, data, *force); err != nil {
		return fail(asAppErr(err))
	}
	written, aerr := writeSegments(*out, m, tracks, *in == "", *seed, *force)
	if aerr != nil {
		return fail(aerr)
	}
	rep, verr := verify.Run(*out, *tol)
	if rep != nil && *reportPath != "" {
		rb, _ := json.MarshalIndent(rep, "", "  ")
		if _, err := pack.WriteAtomic(*reportPath, append(rb, '\n'), *force); err != nil {
			return fail(asAppErr(err))
		}
	}
	fmt.Printf("manifest: %s\nrepresentations: %d\nsegments_written: %d\n", *out, len(m.Periods[0].AdaptationSets), written)
	if rep != nil {
		fmt.Printf("segments_per_rep: %d\nmax_deviation_ms: %d\ndrift_count: %d\n",
			rep.SegmentsPerRep, rep.MaxDeviationMs, rep.DriftCount)
	}
	if verr != nil {
		return fail(asAppErr(verr))
	}
	return errs.ExitOK
}

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	mpdPath := fs.String("mpd", "", "manifest path")
	asJSON := fs.Bool("json", false, "print JSON report")
	tol := fs.Int64("tolerance-ms", 10, "alignment tolerance in ms")
	if err := fs.Parse(args); err != nil || *mpdPath == "" {
		usage()
		return errs.ExitParam
	}
	rep, verr := verify.Run(*mpdPath, *tol)
	if rep != nil {
		if *asJSON {
			rb, _ := json.MarshalIndent(rep, "", "  ")
			fmt.Println(string(rb))
		} else {
			fmt.Printf("manifest: %s\nrepresentations: %d\nsegments_per_rep: %d\nexpand: %s..%s\nmax_deviation_ms: %d (tolerance %d)\ndrift_count: %d\nstatus: %s\n",
				rep.MPD, rep.Representations, rep.SegmentsPerRep, rep.ExpandFirst, rep.ExpandLast,
				rep.MaxDeviationMs, rep.ToleranceMs, rep.DriftCount, rep.Status)
		}
	}
	if verr != nil {
		return fail(asAppErr(verr))
	}
	return errs.ExitOK
}

// loadTracks synthesises deterministic renditions or probes an input tree.
func loadTracks(in string, seed, reps, duration, seg int64) ([]ingest.Track, *errs.AppError) {
	if in == "" {
		tracks, err := ingest.Synthesize(ingest.GenParams{Seed: seed, Reps: reps, DurationSec: duration, SegSec: seg})
		if err != nil {
			return nil, errs.New(errs.ExitParam, errs.CodeParam, err.Error(), nil)
		}
		return tracks, nil
	}
	tracks, err := ingest.ProbeMulti(in)
	if err != nil {
		return nil, errs.New(errs.ExitMissing, errs.CodeSegmentCorrupt, err.Error(), nil)
	}
	return tracks, nil
}

// writeSegments materialises segment files next to the manifest, named by
// template expansion. Synthetic content is rendered; probed input is copied.
func writeSegments(out string, m *mpd.MPD, tracks []ingest.Track, synthetic bool, seed int64, force bool) (int, *errs.AppError) {
	byID := make(map[string]ingest.Track, len(tracks))
	for _, t := range tracks {
		byID[t.ID] = t
	}
	written := 0
	for _, p := range m.Periods {
		for _, as := range p.AdaptationSets {
			for i := range as.Representations {
				r := &as.Representations[i]
				tr, ok := byID[r.ID]
				if !ok {
					return written, errs.New(errs.ExitParam, errs.CodeParam, "no track for representation "+r.ID, nil)
				}
				sn := int64(1)
				if r.SegmentTemplate.StartNumber != nil {
					sn = *r.SegmentTemplate.StartNumber
				}
				pts, err := mpd.ExpandTimeline(r.SegmentTemplate.SegmentTimeline, sn)
				if err != nil {
					return written, errs.New(errs.ExitParam, errs.CodeOverflow, err.Error(), nil)
				}
				if len(pts) != len(tr.Segments) {
					return written, errs.New(errs.ExitMissing, errs.CodeSegmentCount,
						fmt.Sprintf("representation %s: %d timeline points vs %d segments", r.ID, len(pts), len(tr.Segments)), nil)
				}
				dir := filepath.Join(filepath.Dir(out), r.BaseURL)
				for j, pt := range pts {
					name, err := mpd.ExpandTemplate(r.SegmentTemplate.Media, mpd.TemplateVars{
						RepresentationID: r.ID, Bandwidth: r.Bandwidth, Number: pt.Number, Time: pt.T,
					})
					if err != nil {
						return written, errs.New(errs.ExitParam, errs.CodeParam, err.Error(), nil)
					}
					content, aerr := segmentContent(tr, j, pt, synthetic, seed)
					if aerr != nil {
						return written, aerr
					}
					if _, err := pack.WriteAtomic(filepath.Join(dir, name), content, force); err != nil {
						return written, asAppErr(err)
					}
					written++
				}
			}
		}
	}
	return written, nil
}

func segmentContent(tr ingest.Track, j int, pt mpd.SegmentPoint, synthetic bool, seed int64) ([]byte, *errs.AppError) {
	meta := tr.Segments[j]
	if !synthetic {
		data, err := os.ReadFile(filepath.Join(tr.SrcDir, meta.Name))
		if err != nil {
			return nil, errs.New(errs.ExitMissing, errs.CodeMissingSegment, err.Error(), nil)
		}
		return data, nil
	}
	meta.Number = pt.Number
	return ingest.RenderSegment(meta, seed), nil
}

// mapAlignErr converts alignment failures into structured errors.
func mapAlignErr(err error) *errs.AppError {
	var reg *align.TimestampRegression
	if errors.As(err, &reg) {
		return errs.New(errs.ExitAlign, errs.CodeTimestampRegress, err.Error(), nil)
	}
	if errors.Is(err, mpd.ErrOverflow) {
		return errs.New(errs.ExitParam, errs.CodeOverflow, err.Error(), nil)
	}
	return errs.New(errs.ExitParam, errs.CodeParam, err.Error(), nil)
}

func asAppErr(err error) *errs.AppError {
	var ae *errs.AppError
	if errors.As(err, &ae) {
		return ae
	}
	return errs.New(errs.ExitParam, errs.CodeParam, err.Error(), nil)
}

func fail(e *errs.AppError) int {
	fmt.Fprintln(os.Stderr, e.JSON())
	return e.Exit
}
