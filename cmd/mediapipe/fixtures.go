package main

import (
	"mediapipe/internal/testkit"
)

// sampleKind selects which synthetic stream to build.
type sampleKind int

const (
	sampleClean sampleKind = iota
	sampleLate
	sampleTruncated
	sampleInterrupted
)

// sampleBundle wraps one generated stream with the facts the demo reports.
type sampleBundle struct {
	data      []byte
	lateCount int
	corruptAt int64
}

// buildSample regenerates the fixture set for the requested kind. Fixtures are
// deterministic in the demo seed, so every run produces the same bytes.
func buildSample(opts jobOptions, kind sampleKind) sampleBundle {
	clean, late, truncated, interrupted := testkit.Generate(opts.Seed)
	var file testkit.SampleFile
	switch kind {
	case sampleClean:
		file = clean
	case sampleLate:
		file = late
	case sampleTruncated:
		file = truncated
	case sampleInterrupted:
		file = interrupted
	}
	return sampleBundle{
		data:      file.Bytes,
		lateCount: len(late.Late),
		corruptAt: int64(len(truncated.Bytes)),
	}
}

// demoFixtures holds every generated stream for one demo run.
type demoFixtures struct {
	clean       []byte
	late        []byte
	truncated   []byte
	interrupted []byte
	lateSamples int
	corruptAt   int64
}

func buildFixtures(opts jobOptions) demoFixtures {
	clean := buildSample(opts, sampleClean)
	late := buildSample(opts, sampleLate)
	truncated := buildSample(opts, sampleTruncated)
	interrupted := buildSample(opts, sampleInterrupted)
	return demoFixtures{
		clean:       clean.data,
		late:        late.data,
		truncated:   truncated.data,
		interrupted: interrupted.data,
		lateSamples: late.lateCount,
		corruptAt:   truncated.corruptAt,
	}
}
