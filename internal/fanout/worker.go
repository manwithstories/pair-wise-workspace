// Package fanout distributes per-representation segmentation tasks at
// the unified cut points. Each segment is stamped with an RTP
// timestamp on the 90 kHz clock so downstream packetizers share one
// time base; execution is ordered so checkpoints and manifests are
// deterministic.
package fanout

import (
	"fmt"
	"hash/fnv"

	"github.com/pion/rtp"

	"mediac/internal/clock"
	"mediac/internal/model"
)

// Task is one segment of one representation.
type Task struct {
	RepID      string
	Index      int
	Start, End clock.Rational
}

// Result is the outcome of one executed task.
type Result struct {
	Task         Task
	SHA256       string
	Bytes        int64
	RTPTimestamp uint32
}

// Worker executes tasks in submission order.
type Worker struct {
	// After, if set, is called after every completed task (used for
	// checkpointing and interrupt simulation).
	After func(Result)
}

// Run executes every task; the first failure aborts the run and keeps
// the failing task's diagnostic state.
func (w Worker) Run(tasks []Task, fn func(Task) (Result, *model.MediaError)) ([]Result, *model.MediaError) {
	out := make([]Result, 0, len(tasks))
	for _, t := range tasks {
		r, merr := fn(t)
		if merr != nil {
			merr.Representation = t.RepID
			merr.SegmentIndex = t.Index
			return out, merr
		}
		out = append(out, r)
		if w.After != nil {
			w.After(r)
		}
	}
	return out, nil
}

// RTPBase converts a cut point to a 90 kHz RTP timestamp. The
// conversion must be exact; otherwise it fails instead of rounding.
func RTPBase(t clock.Rational) (uint32, *model.MediaError) {
	num := t.Value * 90000
	if num%t.Timescale != 0 {
		return 0, model.New(model.StageFanout, "inexact_rtp",
			fmt.Sprintf("cut %s is not representable on the 90 kHz RTP clock", t), 1)
	}
	return uint32(num / t.Timescale), nil
}

// SSRC derives a deterministic SSRC for a representation.
func SSRC(repID string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(repID))
	return h.Sum32()
}

// Seal prepends a deterministic RTP header to the segment payload.
func Seal(repID string, index int, rtpTS uint32, payload []byte) ([]byte, *model.MediaError) {
	pkt := rtp.Packet{Header: rtp.Header{
		Version:        2,
		Marker:         true,
		PayloadType:    96,
		SequenceNumber: uint16(index),
		Timestamp:      rtpTS,
		SSRC:           SSRC(repID),
	}, Payload: payload}
	b, err := pkt.Marshal()
	if err != nil {
		return nil, model.New(model.StageFanout, "rtp_seal", err.Error(), 1)
	}
	return b, nil
}
