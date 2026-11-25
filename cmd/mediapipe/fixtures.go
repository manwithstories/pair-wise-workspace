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

// buildSample regenerates the fixture set for the requested kind. Fixtures are
// deterministic in the demo seed, so every run produces the same bytes.
func buildSample(opts jobOptions, kind sampleKind) []byte {
	clean, late, truncated, interrupted := testkit.Generate(opts.Seed)
	switch kind {
	case sampleLate:
		return late.Bytes
	case sampleTruncated:
		return truncated.Bytes
	case sampleInterrupted:
		return interrupted.Bytes
	default:
		return clean.Bytes
	}
}

// demoFixtures holds every generated stream for one demo run. The interrupted
// variant is encoded with unknown-length clusters so the parser must resolve
// boundaries without explicit sizes.
type demoFixtures struct {
	clean       []byte
	late        []byte
	truncated   []byte
	interrupted []byte
}

func buildFixtures(opts jobOptions) demoFixtures {
	return demoFixtures{
		clean:       buildSample(opts, sampleClean),
		late:        buildSample(opts, sampleLate),
		truncated:   buildSample(opts, sampleTruncated),
		interrupted: buildSample(opts, sampleInterrupted),
	}
}
