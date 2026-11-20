package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"

	"mediaroute/internal/provenance"
)

// cmdQuery prints provenance entries for a track and time interval.
func cmdQuery(args []string) (int, error) {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	manPath := fs.String("manifest", "", "provenance manifest file")
	track := fs.Uint("track", 0, "track ID (0 = all)")
	fromNs := fs.Int64("from-ns", 0, "interval start, nanoseconds")
	toNs := fs.Int64("to-ns", -1, "interval end, nanoseconds (-1 = unbounded)")
	if err := fs.Parse(args); err != nil {
		return exitUsage, err
	}
	if *manPath == "" {
		return exitUsage, errors.New("query requires --manifest")
	}
	p, err := resolveInput(*manPath)
	if err != nil {
		return exitUsage, err
	}
	man, err := provenance.Load(p)
	if err != nil {
		return exitUsage, err
	}
	entries := man.Query(uint32(*track), *fromNs, *toNs)
	for _, e := range entries {
		raw, err := json.Marshal(e)
		if err != nil {
			return exitUsage, err
		}
		fmt.Println(string(raw))
	}
	fmt.Printf("entries=%d\n", len(entries))
	return exitOK, nil
}
