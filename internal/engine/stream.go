package engine

import (
	"encoding/hex"
	"hash"
	"io"
	"os"
	"time"

	"github.com/pion/rtp"

	"mediaroute/internal/fault"
	"mediaroute/internal/fixture"
	"mediaroute/internal/probe"
	"mediaroute/internal/transport"
)

// StreamConfig controls one streaming/recovery run.
type StreamConfig struct {
	Spec         fixture.Spec
	Report       *probe.ContainerReport
	OutputPath   string
	Checkpoint   string
	StartSeq     uint16
	GapTimeoutNs int64
	// InterruptAfter > 0 simulates a process interruption after this many
	// committed samples; zero runs to completion.
	InterruptAfter int
	// BandwidthMbps is the ordered list of nominal bandwidth stages; the
	// engine cycles through them to exercise pause/resume behaviour.
	BandwidthMbps []float64
}

// Checkpoint is the on-disk resume state. Restart validates all three
// fields: input digest, last committed sample and output prefix hash.
type Checkpoint struct {
	InputSHA256      string `json:"input_sha256"`
	LastSample       int64  `json:"last_sample"` // -1 before first commit
	OutputPrefixHash string `json:"output_prefix_hash"`
	OutputBytes      int64  `json:"output_bytes"`
}

// StreamStats summarises one run.
type StreamStats struct {
	Committed       int
	DamagedSamples  []int64
	Gaps            []transport.Gap
	Duplicates      int
	PausedEvents    int
	OutputSHA256    string
	Interrupted     bool
	ElapsedNanos    int64
	ReplayedSamples int
}

// Stream executes the packet pipeline. On a simulated interruption it
// returns early with a checkpoint on disk and a valid output prefix; the
// caller re-invokes Stream with the same config to resume.
func Stream(cfg StreamConfig) (*StreamStats, error) {
	if cfg.Report == nil || len(cfg.Report.Tracks) == 0 {
		return nil, fault.New(fault.CatBadArgument, "stream requires a scanned report")
	}
	track := &cfg.Report.Tracks[0]
	cp, err := loadCheckpoint(cfg.Checkpoint)
	if err != nil {
		return nil, err
	}
	resuming := cp != nil
	if resuming {
		if err := verifyCheckpoint(cp, cfg); err != nil {
			return nil, err
		}
	} else {
		cp = &Checkpoint{LastSample: -1, InputSHA256: cfg.Report.InputSHA256}
	}
	packets, err := cfg.Spec.BuildPackets(cfg.StartSeq)
	if err != nil {
		return nil, err
	}
	rb := transport.NewReorderBuffer(1, cfg.GapTimeoutNs)
	jb := transport.NewJitterBuffer(rb)
	asm := transport.NewAssembler(cfg.Spec.SampleCount)
	out, hash, startSample, err := openOutput(cfg.OutputPath, cp, resuming)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	stats := &StreamStats{}
	start := time.Now()
	nominal := nominalBps(cfg.BandwidthMbps)
	damagedSet := map[int64]bool{}
	commitFrom := startSample
	for step, p := range packets {
		now := time.Now().UnixNano()
		injectBandwidth(jb, step, nominal, cfg.BandwidthMbps)
		if jb.Paused() {
			stats.PausedEvents++
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(p.Raw); err != nil {
			return nil, fault.New(fault.CatPacketGap, "bad rtp: %v", err)
		}
		if _, ferr := jb.Enqueue(pkt.SequenceNumber, pkt.Payload, now); ferr != nil {
			return nil, ferr
		}
		drainAndCommit(jb, asm, out, hash, track, cp, commitFrom, damagedSet, stats, now)
		for _, g := range jb.ExpireGaps(now) {
			stats.Gaps = append(stats.Gaps, g)
			d := asm.MarkDamage(g)
			markDamageRange(track, d, damagedSet, stats)
		}
		if cfg.InterruptAfter > 0 && stats.Committed >= cfg.InterruptAfter {
			stats.Interrupted = true
			break
		}
	}
	// Final gap sweep for anything still pending.
	finalNow := time.Now().UnixNano() + cfg.GapTimeoutNs + 1
	for _, g := range jb.ExpireGaps(finalNow) {
		stats.Gaps = append(stats.Gaps, g)
		d := asm.MarkDamage(g)
		markDamageRange(track, d, damagedSet, stats)
	}
	drainAndCommit(jb, asm, out, hash, track, cp, commitFrom, damagedSet, stats, finalNow)
	stats.ElapsedNanos = time.Since(start).Nanoseconds()
	stats.OutputSHA256 = hex.EncodeToString(hash.Sum(nil))
	stats.DamagedSamples = sortedKeys(damagedSet)
	stats.Duplicates = rb.Snapshot().Duplicates
	if resuming {
		stats.ReplayedSamples = stats.Committed
	}
	if stats.Interrupted {
		if err := writeCheckpoint(cfg.Checkpoint, cp); err != nil {
			return nil, err
		}
		return stats, nil
	}
	// Clean completion: checkpoint no longer needed.
	os.Remove(cfg.Checkpoint)
	return stats, nil
}

func drainAndCommit(jb *transport.JitterBuffer, asm *transport.Assembler,
	out io.Writer, hash hash.Hash, track *probe.TrackMeta,
	cp *Checkpoint, commitFrom int64, damaged map[int64]bool,
	stats *StreamStats, nowNanos int64) {
	for _, payload := range jb.Dequeue() {
		idx, done, ferr := asm.Ingest(payload)
		if ferr != nil || !done {
			continue
		}
		if int64(idx) <= commitFrom {
			continue // already committed before resume
		}
		if asm.IsDamaged(int64(idx)) {
			continue // never fabricate damaged samples
		}
		data, ok := asm.Materialize(idx)
		if !ok {
			continue
		}
		// Complete sample boundaries only.
		frame := framedSample(idx, data)
		n, _ := out.Write(frame)
		hash.Write(frame)
		cp.LastSample = int64(idx)
		cp.OutputPrefixHash = hex.EncodeToString(hash.Sum(nil))
		cp.OutputBytes += int64(n)
		stats.Committed++
	}
	_ = nowNanos
	_ = track
}

// framedSample wraps one complete sample with a deterministic length
// prefix so output boundaries are recoverable and hashes are stable.
func framedSample(idx uint32, data []byte) []byte {
	frame := make([]byte, 8+len(data))
	frame[0] = byte(idx >> 24)
	frame[1] = byte(idx >> 16)
	frame[2] = byte(idx >> 8)
	frame[3] = byte(idx)
	frame[4] = byte(len(data) >> 24)
	frame[5] = byte(len(data) >> 16)
	frame[6] = byte(len(data) >> 8)
	frame[7] = byte(len(data))
	copy(frame[8:], data)
	return frame
}

func markDamageRange(track *probe.TrackMeta, d transport.SampleDamage,
	set map[int64]bool, stats *StreamStats) {
	first, last := refineDamage(track, d)
	for s := first; s <= last; s++ {
		if !set[s] {
			set[s] = true
			stats.DamagedSamples = append(stats.DamagedSamples, s)
		}
	}
}

// refineDamage projects a packet gap onto the track's sample timeline so
// the damage locator is accurate to within one sample.
func refineDamage(track *probe.TrackMeta, d transport.SampleDamage) (int64, int64) {
	start := track.FindSampleAt(d.Gap.TimeRange.StartNanos)
	end := track.FindSampleAt(d.Gap.TimeRange.EndNanos)
	if start < 0 {
		start = d.SampleFirst
	}
	if end < start {
		end = start
	}
	return start, end
}
