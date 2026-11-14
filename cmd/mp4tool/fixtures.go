package main

import (
	"fmt"

	"mp4tool/internal/fixture"
)

func cmdFixtures(args []string) int {
	fs := newFlagSet("fixtures")
	out := fs.String("out", "fixtures", "output directory")
	if fs.Parse(args) != nil || *out == "" {
		return exitUsage
	}
	names, err := fixture.GenerateAll(*out)
	if err != nil {
		return fail(err)
	}
	for _, n := range names {
		fmt.Printf("generated %s/%s\n", *out, n)
	}
	return exitOK
}
