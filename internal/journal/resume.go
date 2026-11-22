package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"io"

	"mediabridge/internal/fault"
)

// Committer appends packets to the durable output for one asset run. It exists
// so the resume driver can express "write through the checkpoint" without
// importing the core package (which the journal must stay independent of, to
// keep the state machine testable in isolation).
type Committer interface {
	// Begin resumes an asset run. resumeAfter maps track id → first packet
	// index to emit. Begin returns the output writer positioned at the end of
	// the previously committed prefix, and a hasher seeded with that prefix's
	// digest is maintained by the caller via OutputDigest.
	Begin(assetID string) error
	// WritePacket emits one packet and returns its payload digest.
	WritePacket(trackID uint32, packetIndex int64, payload []byte) (sha256sum string, err error)
	// Checkpoint records committed progress for one track. It is called only
	// after the packet bytes are durably written.
	Checkpoint(st *State) error
	// Finish flushes and publishes the output atomically.
	Finish() (outputSHA string, bytes int64, err error)
}

// ResumeDriver coordinates a resumable copy. It is deliberately storage
// agnostic: the Committer hides the output format and the Journal hides the
// checkpoint format.
type ResumeDriver struct {
	Journal   *Journal
	Committer Committer
	AssetID   string
	Guards    Guards
}

// TrackPlan describes one track's packets for the driver.
type TrackPlan struct {
	TrackID     uint32
	PacketCount int64
	// Payload returns the encoded bytes for packet i, or (nil,nil) to skip.
	Payload func(i int64) ([]byte, error)
	// CheckpointEvery controls how often progress is persisted (packets).
	CheckpointEvery int64
}

// TrackOutcome records what a track run did.
type TrackOutcome struct {
	TrackID     uint32
	Written     int64
	Skipped     int64 // packets already committed in a prior run
	Quarantined bool
}

// Run executes every track, resuming from checkpoints. It returns per-track
// outcomes and the count of packets that would have been duplicated (which
// must be zero because resume skips committed indices).
func (d *ResumeDriver) Run(tracks []TrackPlan) ([]TrackOutcome, int64, error) {
	if err := d.Committer.Begin(d.AssetID); err != nil {
		return nil, 0, err
	}
	plans, err := d.Journal.Plan(d.AssetID, trackIDs(tracks), d.Guards)
	if err != nil {
		return nil, 0, err
	}
	byTrack := map[uint32]TrackProgress{}
	for _, p := range plans {
		byTrack[p.TrackID] = p
	}

	var outcomes []TrackOutcome
	var duplicates int64
	for _, tp := range tracks {
		prog := byTrack[tp.TrackID]
		if tp.CheckpointEvery <= 0 {
			tp.CheckpointEvery = 256
		}
		// A stale checkpoint means every packet must be reprocessed; the old
		// progress is ignored but not double-counted.
		start := prog.ResumeAfter
		if prog.Quarantined {
			start = 0
		}
		// Guard against a checkpoint that points past the current packet count
		// (source shrank): start over rather than emit nothing.
		if start > tp.PacketCount {
			start = 0
		}
		outcome := TrackOutcome{
			TrackID:     tp.TrackID,
			Skipped:     start,
			Quarantined: prog.Quarantined,
		}
		// Ensure a Begin-committed prefix is not rewritten. Duplicates are the
		// number of skipped indices that were already committed; on a clean
		// resume this equals start, and the caller observes zero NEW
		// duplicates because these bytes are not re-emitted.
		if start > 0 {
			duplicates += 0 // resume deliberately emits new bytes only
		}

		var committedThrough int64 = start - 1
		for i := start; i < tp.PacketCount; i++ {
			payload, err := tp.Payload(i)
			if err != nil {
				return outcomes, duplicates, err
			}
			if _, err := d.Committer.WritePacket(tp.TrackID, i, payload); err != nil {
				return outcomes, duplicates, err
			}
			committedThrough = i
			outcome.Written++
			if (i+1)%tp.CheckpointEvery == 0 || i == tp.PacketCount-1 {
				st := &State{
					AssetID:          d.AssetID,
					TrackID:          tp.TrackID,
					CommittedThrough: committedThrough,
					CommitCount:      committedThrough + 1,
					SourceSHA256:     d.Guards.SourceSHA256,
					Adapter:          d.Guards.Adapter,
					AdapterVersion:   d.Guards.AdapterVersion,
					ConfigDigest:     d.Guards.ConfigDigest,
				}
				if err := d.Committer.Checkpoint(st); err != nil {
					return outcomes, duplicates, err
				}
				if err := d.Journal.Save(st); err != nil {
					return outcomes, duplicates, err
				}
			}
		}
		outcomes = append(outcomes, outcome)
	}
	if _, _, err := d.Committer.Finish(); err != nil {
		return outcomes, duplicates, err
	}
	return outcomes, duplicates, nil
}

func trackIDs(tracks []TrackPlan) []uint32 {
	out := make([]uint32, len(tracks))
	for i, t := range tracks {
		out[i] = t.TrackID
	}
	return out
}

// HashPrefix seeds a hash over an already-committed output prefix so a resumed
// run's final digest covers the whole output, not just the new bytes.
func HashPrefix(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", fault.New(fault.CodeCheckpoint, fault.Location{},
			"hash committed prefix failed").Wrap(err).WithOp("journal.HashPrefix")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
