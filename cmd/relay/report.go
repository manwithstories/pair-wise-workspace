package main

import (
	"fmt"

	"relay/internal/fixture"
)

// verification holds independently measured demo metrics.
type verification struct {
	roundtrip     float64
	keyHits       int
	keyExpected   int
	hashMatched   int
	hashTotal     int
	resumeMatched int
	resumeTotal   int
}

// printAcceptance emits the deterministic acceptance lines. Every numeric
// value is re-derived from on-disk outputs rather than pipeline counters.
func printAcceptance(sc *fixture.Scenario, accepted, deduped, isolated,
	cycleRejected int, queueBudget, queuePeak int64, res verification) {
	fmt.Printf("generated=%d accepted=%d deduped=%d isolated=%d\n",
		sc.Generated, accepted, deduped, isolated)
	fmt.Printf("roundtrip=%.2f%% keyframe_hit=%d/%d sha256_match=%d/%d "+
		"resume_hash_match=%d/%d\n",
		res.roundtrip, res.keyHits, res.keyExpected,
		res.hashMatched, res.hashTotal,
		res.resumeMatched, res.resumeTotal)
	fmt.Printf("queue_peak_bytes<=%d (observed=%d) cycle_rejected=%d\n",
		queueBudget, queuePeak, cycleRejected)
}
