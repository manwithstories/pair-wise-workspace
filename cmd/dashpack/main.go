// dashpack is a local, reproducible DASH packaging tool: it scans fMP4
// segment directories, aligns representation timelines, generates an MPD
// manifest and verifies cross-representation time alignment.
//
// Exit codes: 0 ok, 2 invalid arguments or manifest, 3 deviation over
// threshold, 4 disk read/write failure.
package main

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"dashpack/internal/align"
	"dashpack/internal/fixture"
	"dashpack/internal/mp4"
	"dashpack/internal/mpd"
	"dashpack/internal/pack"
)

const defaultThresholdMs = 2.0

type cliError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		exitWith("invalid", fmt.Errorf("missing subcommand (scan|gen|pack|expand|verify)"))
	}
	var err error
	code := "invalid"
	switch os.Args[1] {
	case "scan":
		err = cmdScan(os.Args[2:])
	case "gen":
		err = cmdGen(os.Args[2:])
	case "pack":
		err = cmdPack(os.Args[2:])
	case "expand":
		err = cmdExpand(os.Args[2:])
	case "verify":
		code, err = cmdVerify(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		exitWith("invalid", fmt.Errorf("unknown subcommand %q", os.Args[1]))
	}
	if err != nil {
		if code == "invalid" {
			code = classify(err)
		}
		exitWith(code, err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: dashpack <subcommand> [flags]
  scan    --dir DIR                      scan one representation directory
  gen     --out DIR [--seed 42]          generate fixture tree
  pack    --root DIR --out DIR           write manifest.mpd + index.json
  expand  --template T --id ID ...       expand one template
          --mpd FILE --root DIR          expand manifest and check targets
  verify  --mpd FILE --root DIR [--json] verify alignment and report`)
}

func classify(err error) string {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return "io"
	}
	return "invalid"
}

func exitWith(code string, err error) {
	exit := map[string]int{"invalid": 2, "deviation": 3, "io": 4}[code]
	if exit == 0 {
		exit = 2
	}
	body, _ := json.Marshal(map[string]cliError{"error": {Code: code, Message: err.Error()}})
	fmt.Fprintln(os.Stderr, string(body))
	os.Exit(exit)
}

func printJSON(v any) {
	body, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(body))
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func cmdScan(args []string) error {
	fs := newFlagSet("scan")
	dir := fs.String("dir", "", "representation directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return fmt.Errorf("scan: --dir is required")
	}
	tl, err := mp4.ScanRepresentation(*dir)
	if err != nil {
		return err
	}
	printJSON(tl)
	return nil
}

func cmdGen(args []string) error {
	fs := newFlagSet("gen")
	out := fs.String("out", "", "output fixture directory")
	seed := fs.Int64("seed", 42, "fixture random seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("gen: --out is required")
	}
	if err := fixture.Generate(*out, *seed); err != nil {
		return err
	}
	fmt.Printf("generated fixtures in %s (seed %d)\n", *out, *seed)
	return nil
}

func cmdPack(args []string) error {
	fs := newFlagSet("pack")
	root := fs.String("root", "", "fixture root directory")
	out := fs.String("out", "", "output directory")
	threshold := fs.Float64("threshold-ms", defaultThresholdMs, "deviation threshold in ms")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *root == "" || *out == "" {
		return fmt.Errorf("pack: --root and --out are required")
	}
	report, err := pack.Pack(*root, *out, *threshold)
	if err != nil {
		return err
	}
	printJSON(report)
	return nil
}

func cmdExpand(args []string) error {
	fs := newFlagSet("expand")
	tpl := fs.String("template", "", "single template to expand")
	id := fs.String("id", "", "representation id for --template mode")
	number := fs.Uint64("number", 0, "segment number for --template mode")
	timeVar := fs.Uint64("time", 0, "segment time for --template mode")
	bandwidth := fs.Uint64("bandwidth", 0, "bandwidth for --template mode")
	mpdPath := fs.String("mpd", "", "manifest to expand")
	root := fs.String("root", "", "fixture root for --mpd mode")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tpl != "" {
		if *id == "" {
			return fmt.Errorf("expand: --id is required with --template")
		}
		out, err := mpd.Expand(*tpl, mpd.Vars{
			RepresentationID: *id, Number: *number, Time: *timeVar, Bandwidth: *bandwidth,
		})
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	}
	if *mpdPath == "" || *root == "" {
		return fmt.Errorf("expand: --mpd and --root are required")
	}
	return expandManifest(*mpdPath, *root)
}

func expandManifest(mpdPath, root string) error {
	manifest, err := readManifest(mpdPath)
	if err != nil {
		return err
	}
	metas, err := pack.LoadMetas(root)
	if err != nil {
		return err
	}
	dirs := make(map[string]string, len(metas))
	for _, m := range metas {
		dirs[m.ID] = m.Dir
	}
	expanded := 0
	for _, set := range manifest.Period.AdaptationSets {
		for _, rep := range set.Representations {
			dir, ok := dirs[rep.ID]
			if !ok {
				return fmt.Errorf("expand: representation %q not found in fixtures.json", rep.ID)
			}
			st := rep.SegmentTemplate
			vars := mpd.Vars{RepresentationID: rep.ID, Bandwidth: rep.Bandwidth}
			targets := []string{}
			initName, err := mpd.Expand(st.Initialization, vars)
			if err != nil {
				return err
			}
			targets = append(targets, initName)
			if st.SegmentTimeline == nil {
				return fmt.Errorf("expand: representation %q has no SegmentTimeline", rep.ID)
			}
			idx := uint64(0)
			t := uint64(0)
			for si, s := range st.SegmentTimeline.S {
				if si == 0 || s.T != 0 {
					t = s.T
				}
				for r := 0; r <= s.R; r++ {
					vars.Number = st.StartNumber + idx
					vars.Time = t
					name, err := mpd.Expand(st.Media, vars)
					if err != nil {
						return err
					}
					targets = append(targets, name)
					idx++
					t += s.D
				}
			}
			for _, name := range targets {
				full := filepath.Join(root, dir, name)
				if _, err := os.Stat(full); err != nil {
					return fmt.Errorf("expand: target %s: %w", full, err)
				}
				expanded++
			}
		}
	}
	fmt.Printf("expanded %d targets, all present\n", expanded)
	return nil
}

func readManifest(path string) (*mpd.MPD, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var manifest mpd.MPD
	if err := xml.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %v", path, err)
	}
	if len(manifest.Period.AdaptationSets) == 0 {
		return nil, fmt.Errorf("parse manifest %s: no adaptation sets", path)
	}
	return &manifest, nil
}

func cmdVerify(args []string) (string, error) {
	fs := newFlagSet("verify")
	mpdPath := fs.String("mpd", "", "manifest file")
	root := fs.String("root", "", "fixture root directory")
	asJSON := fs.Bool("json", false, "print report as JSON")
	threshold := fs.Float64("threshold-ms", defaultThresholdMs, "deviation threshold in ms")
	if err := fs.Parse(args); err != nil {
		return "invalid", err
	}
	if *mpdPath == "" || *root == "" {
		return "invalid", fmt.Errorf("verify: --mpd and --root are required")
	}
	manifest, err := readManifest(*mpdPath)
	if err != nil {
		return "invalid", err
	}
	metas, err := pack.LoadMetas(*root)
	if err != nil {
		return "", err
	}
	if err := checkManifestIDs(manifest, metas); err != nil {
		return "invalid", err
	}
	tls, err := pack.ScanAll(*root, metas)
	if err != nil {
		return "", err
	}
	al, err := align.Align(metas, tls, *threshold)
	if err != nil {
		return "invalid", err
	}
	expanded, failed, err := pack.ExpandAll(metas, tls)
	if err != nil {
		return "invalid", err
	}
	report := align.BuildReport(metas, tls, al, expanded, failed)
	if *asJSON {
		printJSON(report)
	} else {
		fmt.Printf("representations=%d segments=%d expanded=%d failed=%d\n",
			report.Representations, report.SegmentsTotal, report.TemplateExpanded, report.TemplateFailed)
		fmt.Printf("max_deviation_ms=%.3f mean_deviation_ms=%.3f (threshold %.3f)\n",
			report.MaxDeviationMs, report.MeanDeviationMs, *threshold)
		fmt.Printf("gaps=%d overlaps=%d short_tail=%d non_monotonic=%d reference=%s\n",
			report.Gaps, report.Overlaps, report.ShortTail, report.NonMonotonic, report.Reference)
	}
	if report.MaxDeviationMs > *threshold {
		return "deviation", fmt.Errorf("deviation %.3f ms exceeds threshold %.3f ms, out of tolerance: %v",
			report.MaxDeviationMs, *threshold, report.OutOfTolerance)
	}
	return "", nil
}

func checkManifestIDs(manifest *mpd.MPD, metas []align.RepMeta) error {
	want := make(map[string]bool, len(metas))
	for _, m := range metas {
		want[m.ID] = false
	}
	for _, set := range manifest.Period.AdaptationSets {
		for _, rep := range set.Representations {
			if _, ok := want[rep.ID]; !ok {
				return fmt.Errorf("verify: manifest representation %q not in fixtures.json", rep.ID)
			}
			want[rep.ID] = true
		}
	}
	for id, seen := range want {
		if !seen {
			return fmt.Errorf("verify: representation %q missing from manifest", id)
		}
	}
	return nil
}
