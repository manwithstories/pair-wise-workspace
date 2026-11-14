package main

import (
	"fmt"

	"mp4replay/internal/fixture"
	"mp4replay/internal/mp4atom"
	"mp4replay/internal/report"
)

func init() {
	commands["gen"] = cmdGen
	commands["probe"] = cmdProbe
}

func cmdGen(args []string) (int, error) {
	fs := flagSet("gen")
	outP := fs.String("out", "", "output mp4 path")
	tracks := fs.Int("tracks", 1, "track count")
	frames := fs.Int("frames", 120, "video frames")
	variant := fs.String("variant", "elst-ctts-wrap", "fixture variant")
	moovEnd := fs.Bool("moov-at-end", false, "place moov after mdat")
	co64 := fs.Bool("co64", false, "use 64-bit chunk offsets")
	if err := fs.Parse(args); err != nil {
		return report.ExitArgs, err
	}
	if *outP == "" {
		return report.ExitArgs, report.NewError(report.ExitArgs, "gen", "missing --out")
	}
	path := *outP
	var err error
	if err != nil {
		return report.ExitArgs, err
	}
	err = fixture.Generate(fixture.Options{
		Path: path, Tracks: *tracks, Frames: *frames,
		Variant: fixture.Variant(*variant), MoovAtEnd: *moovEnd,
		UseCo64: *co64,
	})
	if err != nil {
		return report.ExitBoxInvalid, err
	}
	fmt.Printf("generated %s tracks=%d frames=%d variant=%s\n",
		path, *tracks, *frames, *variant)
	return report.ExitOK, nil
}

func cmdProbe(args []string) (int, error) {
	fs := flagSet("probe")
	fileP := fs.String("file", "", "input mp4")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return report.ExitArgs, err
	}
	if *fileP == "" {
		return report.ExitArgs, report.NewError(report.ExitArgs, "probe", "missing --file")
	}
	path := *fileP
	var err error
	if err != nil {
		return report.ExitArgs, err
	}
	m, err := mp4atom.Open(path)
	if err != nil {
		return 0, err
	}
	defer m.Close()
	p := buildProbe(m)
	if *asJSON {
		b, err := report.Marshal(p)
		if err != nil {
			return report.ExitArgs, err
		}
		fmt.Println(string(b))
	} else {
		printProbeText(p)
	}
	return report.ExitOK, nil
}
