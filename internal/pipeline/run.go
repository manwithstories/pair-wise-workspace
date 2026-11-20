// Package pipeline orchestrates manifest parsing, segment parsing,
// timebase normalization, ladder alignment, RTP recovery, and validation.
// Each track is an isolation unit: recoverable faults annotate its
// output, fatal structural faults terminate only that track.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	pionrtp "github.com/pion/rtp"

	"mediamap/internal/align"
	"mediamap/internal/clock"
	"mediamap/internal/fault"
	"mediamap/internal/manifest"
	"mediamap/internal/rtp"
	"mediamap/internal/segment"
	"mediamap/internal/testkit"
)

// Hard input caps.
const (
	MaxSegmentBytes = 64 << 20
	alignTolerance  = 20 * 90 // 20 ms in 90 kHz ticks
	alignFallback   = 80 * 90 // 80 ms
)

// SegmentReport is one segment in the output manifest.
type SegmentReport struct {
	URI       string `json:"uri"`
	Start     int64  `json:"start_ticks"`
	Duration  int64  `json:"duration_ticks"`
	Keyframe  bool   `json:"keyframe"`
	Derived   bool   `json:"derived,omitempty"`
	Corrupt   bool   `json:"corrupt,omitempty"`
	DataBytes int64  `json:"data_bytes"`
}

// RepReport is one representation in the output manifest.
type RepReport struct {
	ID        string          `json:"id"`
	Bandwidth int             `json:"bandwidth"`
	Segments  []SegmentReport `json:"segments"`
}

// TrackReport is one track's output: manifest, alignment, RTP, faults.
type TrackReport struct {
	Name      string         `json:"name"`
	Kind      string         `json:"kind"`
	Status    string         `json:"status"` // ok | degraded | failed
	Start     int64          `json:"start_ticks"`
	End       int64          `json:"end_ticks"`
	Alignment align.Result   `json:"alignment"`
	RTP       rtp.Stats      `json:"rtp"`
	Reps      []RepReport    `json:"representations"`
	Faults    []*fault.Error `json:"faults,omitempty"`
}

// Summary holds the acceptance metrics.
type Summary struct {
	AVSyncMs          float64 `json:"av_sync_ms"`
	PacketRecoveryPct float64 `json:"packet_recovery_pct"`
	P95LatencyMs      float64 `json:"p95_latency_ms"`
	AlignmentErrorMs  float64 `json:"alignment_error_ms"`
	CorruptSegments   int     `json:"corrupt_segments"`
	ManifestHash      string  `json:"manifest_hash"`
}

// Output is the full demo result, written to generated/case.json.
type Output struct {
	Summary Summary        `json:"summary"`
	Tracks  []TrackReport  `json:"tracks"`
	Faults  []*fault.Error `json:"faults"`
}

// RunDemo generates fixtures, processes every track, and returns the
// output plus the process exit code: 0 clean, 2 recoverable damage, 1 on
// fatal structural errors.
func RunDemo(opts testkit.Options) (*Output, int) {
	c := testkit.Generate(opts)
	out := &Output{}
	latencies := make([]float64, 0, len(c.Tracks))
	for _, tf := range c.Tracks {
		start := time.Now()
		tr := processTrack(tf)
		latencies = append(latencies, float64(time.Since(start).Microseconds())/1000)
		out.Tracks = append(out.Tracks, tr)
		out.Faults = append(out.Faults, tr.Faults...)
	}
	s := Summary{
		AVSyncMs:         avSync(out.Tracks),
		P95LatencyMs:     p95(latencies),
		ManifestHash:     ManifestHash(out.Tracks),
		CorruptSegments:  countCorrupt(out.Tracks),
		AlignmentErrorMs: maxAlignment(out.Tracks),
	}
	var rec, lost int
	for _, tr := range out.Tracks {
		rec += tr.RTP.Recovered
		lost += tr.RTP.Lost
	}
	if rec+lost == 0 {
		s.PacketRecoveryPct = 100
	} else {
		s.PacketRecoveryPct = float64(rec) / float64(rec+lost) * 100
	}
	out.Summary = s
	code := 0
	if len(out.Faults) > 0 {
		code = 2
	}
	if fault.List(out.Faults).HasFatal() {
		code = 1
	}
	return out, code
}

// ManifestHash is the sha256 over the canonical JSON of the track
// manifests, making the output reproducible.
func ManifestHash(tracks []TrackReport) string {
	b, _ := json.Marshal(tracks)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func processTrack(tf testkit.TrackFixture) TrackReport {
	tr := TrackReport{Name: tf.Name, Kind: tf.Kind, Status: "ok"}
	var faults fault.List
	var rungs []align.Rung
	var haveRange bool
	for _, rep := range tf.Reps {
		model, mf := manifest.ParseHLSMedia(tf.Name, rep.ID, rep.Playlist)
		faults = append(faults, mf...)
		dataByURI := make(map[string][]byte, len(rep.Segments))
		for _, s := range rep.Segments {
			dataByURI[s.URI] = s.Data
		}
		repRep := RepReport{ID: rep.ID, Bandwidth: rep.Bandwidth}
		rung := align.Rung{ID: rep.ID}
		var anchor int64
		var haveAnchor bool
		var prev int64
		for _, seg := range model.Segments {
			data, ok := dataByURI[seg.URI]
			if !ok {
				faults = append(faults, fault.New(fault.StageVerify, tf.Name, fault.NoOffset, true,
					"manifest references missing segment %q", seg.URI))
				continue
			}
			delete(dataByURI, seg.URI)
			if len(data) > MaxSegmentBytes {
				faults = append(faults, fault.New(fault.StageSegment, tf.Name, fault.NoOffset, false,
					"segment %q exceeds %d-byte cap", seg.URI, MaxSegmentBytes))
				continue
			}
			before := len(faults)
			info, sf := segment.Parse(tf.Name, data)
			faults = append(faults, sf...)
			corrupt := len(faults) > before
			repRep.Segments = append(repRep.Segments, SegmentReport{
				URI: seg.URI, Start: seg.Start, Duration: seg.Duration,
				Keyframe: info.Keyframe, Derived: seg.Derived,
				Corrupt: corrupt, DataBytes: info.DataBytes,
			})
			if info.DecodeTime < 0 {
				continue // cut unknown; alignment will report the gap
			}
			raw := info.DecodeTime
			if !haveAnchor {
				anchor, prev = raw, raw
				haveAnchor = true
			} else {
				prev = clock.Unwrap33(prev, uint64(raw))
			}
			rel := clock.ToTimebase(prev-anchor, tf.Timescale).Ticks()
			cut := model.StartOffset + rel
			rung.Cuts = append(rung.Cuts, align.Cut{
				Ticks: cut, Keyframe: info.Keyframe, Derived: seg.Derived,
			})
			if !haveRange || cut < tr.Start {
				tr.Start = cut
			}
			if end := cut + seg.Duration; !haveRange || end > tr.End {
				tr.End = end
			}
			haveRange = true
		}
		for uri := range dataByURI {
			faults = append(faults, fault.New(fault.StageVerify, tf.Name, fault.NoOffset, true,
				"segment %q not referenced by manifest", uri))
		}
		rungs = append(rungs, rung)
		tr.Reps = append(tr.Reps, repRep)
	}
	tr.Alignment = align.Align(rungs, alignTolerance, alignFallback)
	tr.RTP = recoverRTP(tf, &faults)
	tr.Faults = faults
	switch {
	case faults.HasFatal():
		tr.Status = "failed"
	case len(faults) > 0:
		tr.Status = "degraded"
	}
	return tr
}

func recoverRTP(tf testkit.TrackFixture, faults *fault.List) rtp.Stats {
	rec := rtp.NewRecoverer(rtp.MaxWindow)
	for _, raw := range tf.RTP {
		var pkt pionrtp.Packet
		if err := pkt.Unmarshal(raw); err != nil {
			*faults = append(*faults, fault.New(fault.StageRTP, tf.Name, fault.NoOffset, true,
				"undecodable RTP packet: %v", err))
			continue
		}
		rec.Feed(pkt)
	}
	rec.Flush()
	return rec.Stats()
}

// avSync returns the worst start/end mismatch between any video/audio
// track pair, in milliseconds.
func avSync(tracks []TrackReport) float64 {
	var worst int64
	for _, v := range tracks {
		if v.Kind != "video" || v.Status == "failed" {
			continue
		}
		for _, a := range tracks {
			if a.Kind != "audio" || a.Status == "failed" {
				continue
			}
			if d := abs64(a.Start - v.Start); d > worst {
				worst = d
			}
			if d := abs64(a.End - v.End); d > worst {
				worst = d
			}
		}
	}
	return clock.TicksToMillis(worst)
}

func p95(lat []float64) float64 {
	if len(lat) == 0 {
		return 0
	}
	s := append([]float64(nil), lat...)
	sort.Float64s(s)
	i := (95*len(s) + 99) / 100 // ceil(0.95*n)
	return s[i-1]
}

func countCorrupt(tracks []TrackReport) int {
	n := 0
	for _, tr := range tracks {
		for _, rep := range tr.Reps {
			for _, s := range rep.Segments {
				if s.Corrupt {
					n++
				}
			}
		}
	}
	return n
}

func maxAlignment(tracks []TrackReport) float64 {
	m := 0.0
	for _, tr := range tracks {
		if tr.Alignment.ErrorMs > m {
			m = tr.Alignment.ErrorMs
		}
	}
	return m
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
