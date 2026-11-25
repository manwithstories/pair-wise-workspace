package main

import "fmt"

func printHashes(hashes []string) {
	for i, h := range hashes {
		fmt.Printf("segment=%d sha256=%s\n", i, h)
	}
}

func printMetrics(m Metrics) {
	fmt.Printf(
		"roundtrip=%d/%d keyframe_hit=%d/%d segment_sha256=%d mismatch=%d rejected=%d rtp_duplicate=%d resumed=%d invalidated=%d\n",
		m.RoundtripOK, m.RoundtripTotal,
		m.KeyframeHits, m.KeyframeTotal,
		m.SegmentSHA256, m.Mismatch, m.Rejected,
		m.RTPDuplicate, m.Resumed, m.Invalidated,
	)
}
