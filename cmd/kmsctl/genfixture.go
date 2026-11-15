package main

import (
	"flag"

	"kmsctl/internal/fixture"
)

type genResult struct {
	Shards    int    `json:"shards"`
	ShardSize int    `json:"shard_size"`
	Out       string `json:"out"`
}

func cmdGenFixture(args []string) int {
	fs := flag.NewFlagSet("gen-fixture", flag.ContinueOnError)
	shards := fs.Int("shards", 0, "number of shards to generate")
	shardSize := fs.Int("shard-size", 0, "bytes per shard")
	seed := fs.Int64("seed", 1, "deterministic seed")
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *out == "" || *shards <= 0 || *shardSize <= 0 {
		return fail(exitUsage, errRequired("--out, --shards and --shard-size"))
	}
	m, err := fixture.Generate(fixture.Params{
		Shards:    *shards,
		ShardSize: *shardSize,
		Seed:      *seed,
		Out:       *out,
	})
	if err != nil {
		return fail(exitUsage, err)
	}
	printJSON(genResult{Shards: len(m.Shards), ShardSize: m.ShardSize, Out: *out})
	return exitOK
}

type errString string

func (e errString) Error() string { return string(e) }

func errRequired(what string) error {
	return errString("missing required flags: " + what)
}
