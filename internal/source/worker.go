package source

import (
	"bytes"
	"context"
	"os"
	"sync"

	"github.com/pion/rtp"

	"mediad/internal/core"
	"mediad/internal/mp4"
	"mediad/internal/timeline"
)

// Job is one fragment unit of work.
type Job struct {
	Index       int
	Src         Source
	Timescale   uint32
	RTPClock    uint32
	RTPSidecar  string
}

// Outcome is a worker result. When Fault is set the fragment must be
// quarantined; otherwise Observation is ready for timeline repair.
type Outcome struct {
	Index        int
	Src          Source
	Observation  timeline.Observation
	Fault        *core.Fault
	RoundTripOK  bool
	Parsed       bool
}

// ReadRTPEvidence decodes a single marshaled pion/rtp packet. A missing or
// undecodable sidecar yields Present=false so repair opens an explicit period
// instead of trusting absent evidence.
func ReadRTPEvidence(sidecar string, clockRate uint32) timeline.RTPEvidence {
	if sidecar == "" || clockRate == 0 {
		return timeline.RTPEvidence{}
	}
	b, err := os.ReadFile(sidecar)
	if err != nil {
		return timeline.RTPEvidence{}
	}
	var pkt rtp.Packet
	if err := pkt.Unmarshal(b); err != nil {
		return timeline.RTPEvidence{}
	}
	return timeline.RTPEvidence{Present: true, Timestamp: pkt.Timestamp, ClockRate: clockRate}
}

// AnalyzeOne reads and parses one fragment, performs the byte roundtrip and
// attaches RTP corroboration. It never panics on malformed input.
func AnalyzeOne(j Job) Outcome {
	oc := Outcome{Index: j.Index, Src: j.Src}
	raw, err := os.ReadFile(j.Src.Abs)
	if err != nil {
		oc.Fault = core.WrapFault(core.KindTruncated, j.Src.Rel, -1, err)
		return oc
	}
	if int64(len(raw)) > core.MaxFragmentBytes {
		oc.Fault = core.Faultf(core.KindOutOfBounds, -1, "fragment exceeds 128 MiB")
		oc.Fault.Path = j.Src.Rel
		return oc
	}

	ev := ReadRTPEvidence(j.RTPSidecar, j.RTPClock)
	frag, perr := mp4.ParseFragment(raw)
	if perr != nil {
		if f, ok := core.AsFault(perr); ok {
			f.Path = j.Src.Rel
			oc.Fault = f
		} else {
			oc.Fault = core.WrapFault(core.KindBadField, j.Src.Rel, -1, perr)
		}
		return oc
	}
	oc.Parsed = true

	if reEncoded, err := mp4.EncodeFragment(frag); err == nil {
		oc.RoundTripOK = bytes.Equal(raw, reEncoded)
	}

	samples := make([]timeline.SampleTiming, 0, len(frag.Samples))
	for _, s := range frag.Samples {
		samples = append(samples, timeline.SampleTiming{
			Duration: s.Duration, CTOffset: s.CTOffset, Sync: s.IsSync(),
		})
	}
	oc.Observation = timeline.Observation{
		ID:             j.Src.Rel,
		Digest:         j.Src.SHA256,
		Path:           j.Src.Rel,
		Camera:         j.Src.Camera,
		Representation: j.Src.Representation,
		Sequence:       frag.Sequence,
		HasSequence:    true,
		Timescale:      j.Timescale,
		TFDT:           frag.BaseDecode,
		Samples:        samples,
		RTP:            ev,
	}
	return oc
}

// SpawnWorkers starts n workers consuming jobs. It returns a wait function.
// Both job and result channel sends respect ctx so cancellation cannot strand
// a goroutine; bounded channels provide queue limits and backpressure.
func SpawnWorkers(ctx context.Context, n int, jobs <-chan Job, results chan<- Outcome) func() {
	if n < 1 {
		n = 1
	}
	var wg sync.WaitGroup
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j, ok := <-jobs:
					if !ok {
						return
					}
					oc := AnalyzeOne(j)
					select {
					case results <- oc:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	return wg.Wait
}
