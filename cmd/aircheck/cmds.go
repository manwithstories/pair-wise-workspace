package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"aircheck/internal/fault"
	"aircheck/internal/index"
	"aircheck/internal/normalize"
	"aircheck/internal/plan"
	"aircheck/internal/scan"
	"aircheck/internal/stream"
)

// configString is the canonical normalizer/plan configuration that forms
// part of the cache key.
func configString() string {
	raw, _ := json.Marshal(map[string]any{
		"audio": normalize.DefaultAudioPolicy(),
		"color": normalize.DefaultColorPolicy(),
	})
	return string(raw)
}

func loadDelivery(input string) (*scan.Delivery, error) {
	raw, err := os.ReadFile(filepath.Join(input, "delivery.json"))
	if err != nil {
		return nil, fault.New(fault.CodeMissingTrack, input, -1, false,
			"delivery.json unreadable: %v", err)
	}
	var d scan.Delivery
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fault.New(fault.CodeMalformedEvent, "delivery.json", -1, false,
			"delivery manifest undecodable: %v", err)
	}
	return &d, nil
}

// scanOrLoad returns the cached index when the key matches, else rescans.
func scanOrLoad(input, cacheDir string) (*scan.Result, string, error) {
	key, err := index.KeyFor(input, configString())
	if err != nil {
		return nil, "", err
	}
	store := index.NewStore(cacheDir)
	var res scan.Result
	if err := store.Load("scan", key, &res); err == nil {
		return &res, key, nil
	} else if fe := fault.Of(err); fe != nil && fe.Code == fault.CodeIndexCorrupt {
		return nil, "", err
	}
	del, err := loadDelivery(input)
	if err != nil {
		return nil, "", err
	}
	res2, err := scan.ScanPackage(input, del)
	if err != nil {
		return nil, "", err
	}
	if err := store.Save("scan", key, res2); err != nil {
		return nil, "", err
	}
	return res2, key, nil
}

func cmdScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	input := fs.String("input", "", "delivery package directory")
	cache := fs.String("cache", "", "cache directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *input == "" || *cache == "" {
		return fmt.Errorf("--input and --cache are required")
	}
	res, key, err := scanOrLoad(*input, *cache)
	if err != nil {
		return err
	}
	printJSON(map[string]any{
		"ok": true, "package": res.Package, "key": key,
		"renditions": len(res.Renditions), "events": len(res.Events),
		"segment_ticks": res.SegmentTicks,
	})
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	input := fs.String("input", "", "delivery package directory")
	cache := fs.String("cache", "", "cache directory")
	out := fs.String("out", "", "output directory")
	resume := fs.Bool("resume", false, "reuse verified segments")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *input == "" || *cache == "" || *out == "" {
		return fmt.Errorf("--input, --cache and --out are required")
	}
	res, key, err := scanOrLoad(*input, *cache)
	if err != nil {
		return err
	}
	cp, err := plan.Build(res)
	if err != nil {
		return err
	}
	notes, err := stream.RunAll(res, cp, *input, key, stream.Options{
		OutDir: *out, Resume: *resume,
		Audio: normalize.DefaultAudioPolicy(), Color: normalize.DefaultColorPolicy(),
	})
	for _, n := range notes {
		fmt.Fprintln(os.Stderr, "normalize:", n)
	}
	if err != nil {
		return err
	}
	printJSON(map[string]any{"ok": true, "out": *out, "segments": cp.NumSegments(),
		"events": len(cp.Events), "notes": len(notes)})
	return nil
}

func probeBitrate(path string) (int64, error) {
	cmd := execFFProbe(path)
	bps, err := strconv.ParseInt(strings.TrimSpace(cmd), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bitrate probe of %s: %q", path, cmd)
	}
	return bps, nil
}
