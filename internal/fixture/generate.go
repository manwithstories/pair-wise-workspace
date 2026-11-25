// Package fixture deterministically generates all media inputs used by demos and tests.
package fixture

import (
	"fmt"

	"mediqc/internal/media"
	"mediqc/internal/remux"
)

const (
	ModeTS    = "ts"
	ModeRTPTS = "rtp-ts"

	ClassClean    = "clean"
	ClassRepaired = "repaired"
	ClassRejected = "rejected"
)

// Case is one generated broadcast-quality verification scenario.
type Case struct {
	ID               string         `json:"id"`
	Order            int            `json:"order"`
	Title            string         `json:"title"`
	Seed             int64          `json:"seed"`
	InputMode        string         `json:"input_mode"`
	TS               []byte         `json:"-"`
	RTPPackets       [][]byte       `json:"-"`
	Tracks           []media.Track  `json:"tracks"`
	SourceSamples    []media.Sample `json:"-"`
	TargetBitrateBPS int64          `json:"target_bitrate_bps"`
	ExpectedClass    string         `json:"expected_class"`
	WantCCGaps       int            `json:"want_cc_gaps"`
	WantPSISwitches  int            `json:"want_psi_switches"`
	WantPCRWraps     int            `json:"want_pcr_wraps"`
	WantLateReplays  int            `json:"want_late_replays"`
	TraceID          string         `json:"trace_id"`
}

// Suite builds the canonical six deterministic cases for seed.
func Suite(seed int64) ([]*Case, *media.Error) {
	tracks := standardTracks()
	specs := []struct {
		id, title, class, mode string
		vfr, wrap              bool
		cc, psi, pcr, late     int
	}{
		{"clean", "clean H.264/AAC transport stream", ClassClean, ModeRTPTS, false, false, 0, 0, 0, 0},
		{"pcr-wrap", "PCR/PTS/DTS epoch wraparound with drift correction", ClassRepaired, ModeTS, false, true, 0, 0, 1, 0},
		{"cc-jump", "continuity counter discontinuity", ClassRepaired, ModeTS, false, false, 1, 0, 0, 0},
		{"psi-update", "corrupt then valid PMT version update", ClassRepaired, ModeTS, false, false, 0, 1, 0, 0},
		{"late-vfr", "late variable-frame-rate event replay", ClassRepaired, ModeRTPTS, true, false, 0, 0, 0, 1},
		{"truncated", "truncated transport stream and PES", ClassRejected, ModeTS, false, false, 0, 0, 0, 0},
	}

	cases := make([]*Case, 0, len(specs))
	for i, sp := range specs {
		samples := makeSamples(seed+int64(i)*1009, sp.id, sp.vfr, sp.wrap)
		muxSamples := interleaveByDecodeTime(samples)
		ts, merr := remux.BuildTS(tracks, muxSamples, remux.TSOptions{})
		if merr != nil {
			return nil, merr
		}
		c := &Case{
			ID: fmt.Sprintf("%02d-%s", i+1, sp.id), Order: i + 1, Title: sp.title,
			Seed: seed, InputMode: sp.mode, Tracks: append([]media.Track(nil), tracks...),
			SourceSamples: muxSamples, TargetBitrateBPS: 1_056_000,
			ExpectedClass: sp.class, WantCCGaps: sp.cc, WantPSISwitches: sp.psi,
			WantPCRWraps: sp.pcr, WantLateReplays: sp.late,
			TraceID: fmt.Sprintf("trace-%s-%d", sp.id, seed),
		}

		switch sp.id {
		case "pcr-wrap":
			c.TS = injectPCRWrap(ts)
		case "cc-jump":
			c.TS = withCCJump(ts, 0x0100)
		case "psi-update":
			c.TS = withPSIUpdate(ts, tracks, muxSamples)
		case "truncated":
			c.TS = ts[:len(ts)-37]
		default:
			c.TS = ts
		}

		if sp.mode == ModeRTPTS {
			pkts, perr := packetizeRTPTS(c.TS, seed+int64(i)*77)
			if perr != nil {
				return nil, perr
			}
			c.RTPPackets = pkts
		}
		cases = append(cases, c)
	}
	return cases, nil
}
