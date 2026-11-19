package main

import (
	"crypto/sha256"
	"time"

	"archive/internal/mapping"
	"archive/internal/mp4"
	"archive/internal/report"
)

func elapsedMs(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000
}

// countSamples totals the demuxed samples across all segments.
func countSamples(demuxed [][]mp4.Track) int {
	n := 0
	for _, tracks := range demuxed {
		for _, t := range tracks {
			n += len(t.Samples)
		}
	}
	return n
}

// payloadHash combines per-track payload hashes in track order; it must
// match remux.Result.PayloadHash for a lossless run.
func payloadHash(tracks []mp4.Track) [sha256.Size]byte {
	final := sha256.New()
	for _, t := range tracks {
		h := sha256.New()
		for _, s := range t.Samples {
			h.Write(s.Data)
		}
		final.Write(h.Sum(nil))
	}
	var out [sha256.Size]byte
	copy(out[:], final.Sum(nil))
	return out
}

// expectedPayloadHash hashes the input payloads per output track (mapped
// order, all segments), matching what a lossless remux must produce.
func expectedPayloadHash(demuxed [][]mp4.Track) [sha256.Size]byte {
	final := sha256.New()
	for _, m := range mapping.Map(demuxed[0]) {
		h := sha256.New()
		for _, segTracks := range demuxed {
			for _, t := range segTracks {
				if t.ID == m.SourceID {
					for _, s := range t.Samples {
						h.Write(s.Data)
					}
				}
			}
		}
		final.Write(h.Sum(nil))
	}
	var out [sha256.Size]byte
	copy(out[:], final.Sum(nil))
	return out
}

// syncErrors measures |video PTS - nearest audio PTS| in milliseconds.
func syncErrors(tracks []mp4.Track) []float64 {
	var video, audio *mp4.Track
	for i := range tracks {
		switch tracks[i].Type {
		case "video":
			if video == nil {
				video = &tracks[i]
			}
		case "audio":
			if audio == nil {
				audio = &tracks[i]
			}
		}
	}
	if video == nil || audio == nil {
		return nil
	}
	pts := func(t *mp4.Track) []int64 {
		out := make([]int64, len(t.Samples))
		for i, s := range t.Samples {
			out[i] = s.DTS + int64(s.CTSOffset)
		}
		return out
	}
	return report.AVSyncErrors(pts(video), pts(audio))
}
