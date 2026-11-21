// Command fixture generates the deterministic six-camera sample dataset the
// acceptance run consumes. Everything is produced from code: no external
// media files and no network access.
//
// Usage:
//
//	fixture -out DIR [-frames N] [-corrupt-every N] [-key-every N]
//	        [-cameras N] [-bitrates 360p,720p,1080p]
//
// Exit codes: 0 success, 2 invalid parameters, 1 write failure.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/mediamend/mediamend/internal/pairflow"
	"github.com/mediamend/mediamend/internal/segment"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	out := fs.String("out", "", "output directory (required)")
	frames := fs.Uint("frames", 1200, "frames per camera")
	corruptEvery := fs.Uint("corrupt-every", 0, "corrupt every Nth frame's digest (0 disables)")
	keyEvery := fs.Uint("key-every", 30, "keyframe cadence in frames")
	cameras := fs.Int("cameras", 6, "number of cameras (1..6)")
	bitrates := fs.String("bitrates", "360p,720p,1080p", "delivery representations")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "fixture: -out is required")
		return 2
	}
	if *frames == 0 || *cameras <= 0 || *cameras > 6 || *keyEvery == 0 {
		fmt.Fprintln(os.Stderr, "fixture: invalid -frames/-cameras/-key-every")
		return 2
	}
	ladder, err := segment.ParseLadder(*bitrates)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	m, faults := pairflow.Generate(pairflow.Options{
		Root:         *out,
		Frames:       uint32(*frames),
		CorruptEvery: uint32(*corruptEvery),
		KeyEvery:     uint32(*keyEvery),
		Ladder:       ladder,
		CameraCount:  *cameras,
	})
	if faults.Fatal() {
		fmt.Fprintln(os.Stderr, "fixture:", faults.Error())
		return 1
	}
	fmt.Printf("fixture_dir=%s\n", *out)
	fmt.Printf("cameras=%d frames_per_camera=%d key_every=%d\n", len(m.Cameras), *frames, *keyEvery)
	fmt.Printf("bitrates=%s\n", segment.FormatLadderNames(ladder))
	fmt.Printf("corrupt_records=%d\n", m.TotalCorrupt())
	fmt.Printf("boundaries=%s\n", m.Files["boundaries"])
	return 0
}
