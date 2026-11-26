// Package recover maintains independent per-track recovery checkpoints.
package recover

import (
	"math/rand"

	"mediad/internal/index"
	"mediad/internal/media"
)

// Config controls deterministic burst-loss simulation.
type Config struct {
	LossRate          float64 `json:"loss_rate"`
	Burst             int     `json:"burst"`
	MaxRetransmits    int     `json:"max_retransmits"`
	RetransmitSuccess float64 `json:"retransmit_success"`
	// FileSize, when positive, enables sample interval bounds checks. Offset
	// corruption that points outside the file then blocks track recovery.
	FileSize int64 `json:"file_size,omitempty"`
}

// DefaultConfig gives mobile recovery enough retransmissions before IDR fallback.
func DefaultConfig(loss float64, burst int) Config {
	return Config{LossRate: loss, Burst: burst, MaxRetransmits: 2, RetransmitSuccess: 0.94}
}

// PacketStat is the recovery audit for one sample/packet.
type PacketStat struct {
	Sequence        int    `json:"sequence"`
	SampleNumber    uint32 `json:"sample_number"`
	Lost            bool   `json:"lost"`
	Retransmissions int    `json:"retransmissions"`
	Recovered       bool   `json:"recovered"`
	Unrecovered     bool   `json:"unrecovered"`
	Source          string `json:"source,omitempty"`
	LatencyMS       int    `json:"latency_ms"`
}

// Report is one track's independent recovery result.
type Report struct {
	TrackID            uint32         `json:"track_id"`
	Packets            int            `json:"packets"`
	LostPackets        int            `json:"lost_packets"`
	RecoveredPackets   int            `json:"recovered_packets"`
	UnrecoveredPackets int            `json:"unrecovered_packets"`
	Retransmissions    int            `json:"retransmissions"`
	RecoveryPct        float64        `json:"recovery_pct"`
	Stats              []PacketStat   `json:"stats,omitempty"`
	Faults             []*media.Fault `json:"faults,omitempty"`
}

// Checkpoint is a independently recoverable sync position for one track.
type Checkpoint struct {
	TrackID      uint32 `json:"track_id"`
	SampleNumber uint32 `json:"sample_number"`
	Offset       int64  `json:"offset"`
	TimeNS       int64  `json:"time_ns"`
	Verified     bool   `json:"verified"`
}

// Engine keeps one track's checkpoints independent from other tracks.
type Engine struct {
	TrackID     uint32
	Checkpoints []Checkpoint
}

// NewEngine derives checkpoints from verified index points for one track.
func NewEngine(trackID uint32, idx *index.Index) *Engine {
	e := &Engine{TrackID: trackID}
	if idx == nil || idx.Tracks[trackID] == nil {
		return e
	}
	for _, p := range idx.Tracks[trackID].Points {
		if p.Verified {
			e.Checkpoints = append(e.Checkpoints, Checkpoint{
				TrackID: trackID, SampleNumber: p.SampleNumber, Offset: p.Offset,
				TimeNS: p.TimeNS, Verified: true,
			})
		}
	}
	return e
}

// NearestCheckpoint returns the latest verified checkpoint at or before n.
func (e *Engine) NearestCheckpoint(n uint32) (Checkpoint, bool) {
	var best Checkpoint
	ok := false
	for _, c := range e.Checkpoints {
		if c.SampleNumber <= n && c.Verified {
			best, ok = c, true
		}
	}
	return best, ok
}

// RunTrack simulates burst loss and recovery for one track. It never depends on
// another track's checkpoint state.
func RunTrack(t *media.Track, idx *index.Index, rng *rand.Rand, cfg Config) *Report {
	rep := &Report{TrackID: trackID(t)}
	if t == nil {
		rep.Faults = append(rep.Faults, media.NewFault(media.CodeRecoveryFailed, "nil track").Fatal())
		return rep
	}
	engine := NewEngine(t.ID, idx)
	burstLeft := 0
	for i, s := range t.Samples {
		rep.Packets++
		lost := false
		if burstLeft > 0 {
			lost = true
			burstLeft--
		} else if cfg.LossRate > 0 && rng.Float64() < cfg.LossRate {
			lost = true
			if cfg.Burst > 1 {
				burstLeft = cfg.Burst - 1
			}
		}
		stat := PacketStat{Sequence: i + 1, SampleNumber: s.Index, Lost: lost, LatencyMS: 42 + (i%7)*4}
		if !lost {
			rep.Stats = append(rep.Stats, stat)
			continue
		}

		rep.LostPackets++
		recovered := false
		for attempt := 1; attempt <= maxInt(cfg.MaxRetransmits, 1); attempt++ {
			rep.Retransmissions++
			stat.Retransmissions++
			stat.LatencyMS += 28 + attempt*9
			if rng.Float64() < cfg.RetransmitSuccess {
				recovered = true
				stat.Source = "retransmission"
				break
			}
		}

		offsetValid := s.Offset >= 0
		if offsetValid && cfg.FileSize > 0 {
			end, ok := media.ByteRange{Offset: s.Offset, Length: int64(s.Size)}.End()
			offsetValid = ok && end <= cfg.FileSize
		}
		if !offsetValid {
			// A retransmission can recover packet bytes, but a corrupt sample
			// offset means the track cannot place and verify them. This makes
			// offset-table corruption affect recovery explicitly.
			recovered = false
			stat.Unrecovered = true
			rep.UnrecoveredPackets++
			f := media.NewFault(media.CodeRecoveryFailed, "sample %d has invalid sample offset %d", s.Index, s.Offset).
				WithTrack(t.ID).WithBox("stco")
			if s.Offset >= 0 {
				f.WithByteRange(s.Offset, int64(s.Size))
			}
			if clock, err := t.Clock(); err == nil {
				f.WithTimeSpan(s.DTS, s.PTS, clock)
			}
			f.Fatal().WithBasis("rebuild stco before publishing recovery")
			rep.Faults = append(rep.Faults, f)
		} else if !recovered {
			cp, ok := engine.NearestCheckpoint(s.Index)
			if !ok {
				stat.Unrecovered = true
				rep.UnrecoveredPackets++
				f := media.NewFault(media.CodeRecoveryFailed, "no verified sync checkpoint for sample %d", s.Index).
					WithTrack(t.ID).WithBox("stss/stco").
					WithByteRange(s.Offset, int64(s.Size))
				if clock, err := t.Clock(); err == nil {
					f.WithTimeSpan(s.DTS, s.PTS, clock)
				}
				f.Fatal().WithBasis("nearest valid sync point unavailable")
				rep.Faults = append(rep.Faults, f)
			} else {
				recovered = true
				stat.Source = "sync-checkpoint"
				stat.LatencyMS += 46 + int(s.Index-cp.SampleNumber)*2
			}
		}

		stat.Recovered = recovered
		if recovered {
			rep.RecoveredPackets++
		}
		rep.Stats = append(rep.Stats, stat)
	}
	if rep.LostPackets > 0 {
		rep.RecoveryPct = 100 * float64(rep.RecoveredPackets) / float64(rep.LostPackets)
	} else {
		rep.RecoveryPct = 100
	}
	return rep
}

func trackID(t *media.Track) uint32 {
	if t == nil {
		return 0
	}
	return t.ID
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
