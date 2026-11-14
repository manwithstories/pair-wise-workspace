package main

import (
	"flag"
	"path/filepath"

	"segctl/internal/fixture"
)

func runFixture(args []string) error {
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	out := fs.String("out", "testdata", "output root; raw captures land in <out>/raw/<rendition>")
	minutes := fs.Int("minutes", 6, "program length per rendition in minutes")
	seed := fs.Int64("seed", 7, "fault-injection random seed")
	inject := fs.String("inject", "", "faults: loss=N,dup=N,reorder=N,rebase=N")
	if err := fs.Parse(args); err != nil {
		return err
	}
	inj, err := fixture.ParseInject(*inject)
	if err != nil {
		return err
	}
	return fixture.Generate(filepath.Join(*out, "raw"), *minutes, *seed, inj)
}
