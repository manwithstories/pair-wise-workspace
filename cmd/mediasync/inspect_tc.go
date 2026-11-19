package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"mediasync/internal/timeline"
)

// runTimecode converts a caller-supplied timecode list onto the shared
// timeline. Unconvertible labels are reported with offsets and yield exit 2.
func runTimecode(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "mediasync: timecode requires a file of labels and --frames")
		return 1
	}
	var path, frames string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--frames":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "mediasync: --frames requires a value")
				return 1
			}
			frames = args[i+1]
			i++
		default:
			if strings.HasPrefix(args[i], "--frames=") {
				frames = strings.TrimPrefix(args[i], "--frames=")
				continue
			}
			if path == "" {
				path = args[i]
				continue
			}
			fmt.Fprintf(stderr, "mediasync: unexpected argument %q\n", args[i])
			return 1
		}
	}
	if frames == "" {
		fmt.Fprintln(stderr, "mediasync: timecode requires --frames RATE")
		return 1
	}
	rate, err := timeline.ParseRate(frames)
	if err != nil {
		fmt.Fprintf(stderr, "mediasync: %v\n", err)
		return 1
	}
	data, err := readLimited(path, 1<<20)
	if err != nil {
		fmt.Fprintf(stderr, "mediasync: %s: %v\n", path, err)
		return 1
	}
	res := timeline.ParseTimecodeStream(data, rate)
	for _, is := range res.Issues {
		fmt.Fprintf(stdout, "recover %s\n", is.Error())
	}
	fmt.Fprintf(stdout, "%s: rate=%s drop=%v accepted=%d rejected=%d rollovers=%d duplicates=%d\n",
		filepath.Base(path), rate, res.Drop, res.Stats.Accepted, res.Stats.Rejected,
		res.Stats.Wrapped, res.Stats.Duplicate)
	if res.Stats.Rejected > 0 {
		return 2
	}
	return 0
}
