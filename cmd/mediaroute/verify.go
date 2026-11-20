package main

import (
	"os"

	"mediaroute/internal/index"
	"mediaroute/internal/provenance"
)

// damageKinds are manifest error kinds that locate corrupted media.
var damageKinds = map[string]bool{
	"loss": true, "conflict": true, "late": true, "reorder": true,
	"backward": true, "drift": true, "wrap": true, "truncated-nal": true,
}

// minDamageSeq returns the smallest extended sequence number among
// located damage errors in the manifest.
func minDamageSeq(man *provenance.Manifest) uint64 {
	min := ^uint64(0)
	for _, e := range man.Errors {
		if damageKinds[e.Kind] && e.ExtSeq < min {
			min = e.ExtSeq
		}
	}
	if min == ^uint64(0) {
		return 0
	}
	return min
}

// randomAccessChecks parses the output MP4 and verifies that every
// fragment starts on a video keyframe and that per-track decode times
// are non-decreasing. It returns the number of checks run and failed.
func randomAccessChecks(path string, man *provenance.Manifest) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	frags, err := index.Inspect(f)
	if err != nil {
		return 0, 0, err
	}
	var videoID uint32
	for _, e := range man.Entries {
		if e.Kind == "keyframe" {
			videoID = e.TrackID
			break
		}
	}
	checks, fails := 0, 0
	lastTfdt := map[uint32]uint64{}
	for _, frag := range frags {
		if videoID != 0 {
			checks++
			if !frag.FirstSync[videoID] {
				fails++
			}
		}
		for id, dt := range frag.DecodeTime {
			checks++
			if prev, ok := lastTfdt[id]; ok && dt < prev {
				fails++
			}
			lastTfdt[id] = dt
		}
	}
	return checks, fails, nil
}
