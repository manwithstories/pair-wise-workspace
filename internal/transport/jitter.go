package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"pairflow/internal/model"
)

// PushDisposition explains how a pushed packet was treated.
type PushDisposition string

const (
	PushAccepted  PushDisposition = "accepted"
	PushBuffered  PushDisposition = "buffered"
	PushDuplicate PushDisposition = "duplicate"
	PushLate      PushDisposition = "late"
	PushConflict  PushDisposition = "conflict"
)

// PushResult is returned for every ingress packet.
type PushResult struct {
	Disposition PushDisposition
	Error       *model.MediaError
}

// JitterConfig configures deterministic deadline release.
type JitterConfig struct {
	// BaseSequence is the first media sequence expected.
	BaseSequence uint16
	// StartNS is the media playout origin.
	StartNS int64
	// PacketDurationNS drives per-sequence missing deadlines.
	PacketDurationNS int64
	// PlayoutDelayNS is added to every sequence deadline.
	PlayoutDelayNS int64
}

type bufferedPacket struct {
	obs ObservedPacket
}

// JitterBuffer reorders packets and records duplicates, late packets and
// same-sequence/different-content conflicts. It never silently overwrites a
// sequence.
type JitterBuffer struct {
	cfg JitterConfig

	next     uint16
	started  bool
	packets  map[uint16]bufferedPacket
	digests  map[uint16]string
	released map[uint16]bool

	Duplicates []uint16
	Late       []uint16
	Conflicts  []*model.MediaError
	Gaps       []model.Interval
}

// NewJitterBuffer constructs an empty buffer.
func NewJitterBuffer(cfg JitterConfig) *JitterBuffer {
	if cfg.PacketDurationNS <= 0 {
		cfg.PacketDurationNS = 10_000_000
	}
	if cfg.PlayoutDelayNS < 0 {
		cfg.PlayoutDelayNS = 0
	}
	return &JitterBuffer{
		cfg:      cfg,
		next:     cfg.BaseSequence,
		packets:  map[uint16]bufferedPacket{},
		digests:  map[uint16]string{},
		released: map[uint16]bool{},
	}
}

func digestPayload(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Push inserts one observed packet. A conflict is returned as both a result
// error and a non-nil Go error so callers cannot accidentally ignore it.
func (j *JitterBuffer) Push(obs ObservedPacket) (PushResult, error) {
	seq := obs.Sequence()
	digest := digestPayload(obs.Packet.Payload)

	if prior, ok := j.digests[seq]; ok {
		if prior == digest {
			j.Duplicates = append(j.Duplicates, seq)
			return PushResult{Disposition: PushDuplicate}, nil
		}
		me := &model.MediaError{
			Code:    model.CodeSeqConflict,
			Message: fmt.Sprintf("sequence %d reused with different payload", seq),
			Impact: model.Interval{
				StartSeq: int64(seq),
				EndSeq:   int64(seq),
			},
			ExpectedDigest: prior,
			ActualDigest:   digest,
			Recoverable:    false,
		}
		j.Conflicts = append(j.Conflicts, me)
		return PushResult{Disposition: PushConflict, Error: me}, me
	}

	j.digests[seq] = digest
	if !j.started {
		j.started = true
		j.next = j.cfg.BaseSequence
	}

	// Sequence numbers in these local samples stay far from the 16-bit wrap.
	if int16(seq-j.next) < 0 || j.released[seq] {
		j.Late = append(j.Late, seq)
		me := &model.MediaError{
			Code:        model.CodeLatePacket,
			Message:     fmt.Sprintf("packet seq=%d arrived after playout cutoff", seq),
			Impact:      model.Interval{StartSeq: int64(seq), EndSeq: int64(seq)},
			Recoverable: true,
		}
		return PushResult{Disposition: PushLate, Error: me}, me
	}

	j.packets[seq] = bufferedPacket{obs: obs}
	if seq == j.next {
		return PushResult{Disposition: PushAccepted}, nil
	}
	return PushResult{Disposition: PushBuffered}, nil
}

func (j *JitterBuffer) deadline(seq uint16) int64 {
	delta := int64(seq) - int64(j.cfg.BaseSequence)
	return j.cfg.StartNS + delta*j.cfg.PacketDurationNS + j.cfg.PlayoutDelayNS
}

// DrainUntil releases contiguous packets whose deadline has arrived. Missing
// sequences at or before nowNS become explicit gaps and advance the cursor.
func (j *JitterBuffer) DrainUntil(nowNS int64) []ObservedPacket {
	if !j.started {
		return nil
	}
	var out []ObservedPacket
	for {
		if pkt, ok := j.packets[j.next]; ok {
			out = append(out, pkt.obs)
			j.released[j.next] = true
			delete(j.packets, j.next)
			j.next++
			continue
		}
		if nowNS < j.deadline(j.next) {
			break
		}
		seq := j.next
		gap := model.Interval{
			StartNS:  j.deadline(seq) - j.cfg.PacketDurationNS,
			EndNS:    j.deadline(seq),
			StartSeq: int64(seq),
			EndSeq:   int64(seq),
		}
		j.Gaps = append(j.Gaps, gap)
		j.next++
	}
	return out
}

// MissingSequences returns gap sequence numbers collected so far.
func (j *JitterBuffer) MissingSequences() []uint16 {
	out := make([]uint16, 0, len(j.Gaps))
	for _, g := range j.Gaps {
		out = append(out, uint16(g.StartSeq))
	}
	return out
}
