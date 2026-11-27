package fixture

// Scenario is a fully generated, self-describing relay input.
type Scenario struct {
	Manifest Manifest
	// CMAF is the fMP4 fragment byte stream as it should exist after a
	// successful recovery path (or before truncation on the damage path).
	CMAF []byte
	// Packets is the wire-order RTP stream after permutation, duplication
	// and loss injection. FEC parity packets carry IsFEC=true.
	Packets []Packet
	// FECBlocks are the local parity blocks used for single-loss recovery.
	FECBlocks []FECSpec
	// DamagedCMAF, when non-nil, is the truncated/garbled fragment stream
	// the damaged path must reject at ExpectedDamageOffset.
	DamagedCMAF []byte
}

// FECSpec describes one XOR parity block's covered sequence range.
type FECSpec struct {
	FirstSeq uint16
	LastSeq  uint16
	Parity   []byte
}

const (
	videoSSRC = uint32(0x11110000)
	audioSSRC = uint32(0x22220000)
	groupSize = 4 // packets per local XOR FEC group
)

// Generate builds the named scenario deterministically from seed.
func Generate(caseName string, seed int64) (*Scenario, error) {
	switch caseName {
	case CaseDriftWrapVFR:
		return genGood(seed), nil
	case CaseTruncateMdat:
		return genDamaged(seed), nil
	default:
		return nil, &UnknownCaseError{Name: caseName}
	}
}

// UnknownCaseError flags a fixture --case value the generator does not know.
type UnknownCaseError struct{ Name string }

func (e *UnknownCaseError) Error() string {
	return "fixture: unknown case " + e.Name
}

// genGood builds the recoverable path: reordered+duplicate packets, one
// single (FEC-repairable) loss, 33-bit tfdt wrap, node drift and VFR.
func genGood(seed int64) *Scenario {
	pb := newPlanBuilder(seed)
	videoTrack := vfrVideoTrack()
	audioTrack := audioTrack()

	// Place the video base media time near the 33-bit modulus so a later
	// fragment legitimately wraps, exercising unwrap rather than rollback.
	const mod33 = uint64(1) << 33
	videoTrack.BaseTfdt = mod33 - ticksToWrapMargin(videoTrack.Timescale)
	// Align the audio presentation start to the same master-clock instant:
	// both tracks must begin at the same millisecond despite differing
	// timescales, or lip-sync would reject an otherwise healthy stream.
	audioTrack.BaseTfdt = videoTrack.BaseTfdt * uint64(audioTrack.Timescale) / uint64(videoTrack.Timescale)

	cmaf := buildMultiFragmentStream(videoTrack, audioTrack)
	pkts, fec, plan := buildWire(pb, cmaf, seed, lossModeSingle)

	return &Scenario{
		Manifest: Manifest{
			Case:         CaseDriftWrapVFR,
			Seed:         seed,
			Tracks:       []TrackSpec{videoTrack, audioTrack},
			SequencePlan: plan,
		},
		CMAF:      cmaf,
		Packets:   pkts,
		FECBlocks: fec,
	}
}

// genDamaged builds the unrecoverable path: a burst loss inside one FEC
// group leaves a hole, and the resulting mdat is truncated at a precise
// offset the pipeline must locate with delta 0.
func genDamaged(seed int64) *Scenario {
	pb := newPlanBuilder(seed + 1)
	videoTrack := vfrVideoTrack()
	audioTrack := audioTrack()
	const mod33 = uint64(1) << 33
	videoTrack.BaseTfdt = mod33 - ticksToWrapMargin(videoTrack.Timescale)
	audioTrack.BaseTfdt = videoTrack.BaseTfdt * uint64(audioTrack.Timescale) / uint64(videoTrack.Timescale)
	cmaf := buildMultiFragmentStream(videoTrack, audioTrack)

	pkts, fec, plan := buildWire(pb, cmaf, seed, lossModeBurst)

	// The burst removes bytes carried by the lost packets; cut the affected
	// mdat while recording the exact box start the parser must anchor on.
	dp := locateDamage(cmaf, plan.BurstLosses)
	damaged := TruncateMdat(cmaf, dp.cutLength)

	return &Scenario{
		Manifest: Manifest{
			Case:                 CaseTruncateMdat,
			Seed:                 seed,
			Tracks:               []TrackSpec{videoTrack, audioTrack},
			SequencePlan:         plan,
			ExpectedDamageOffset: dp.expectedOffset,
			ExpectBlocked:        "pack",
			ExpectUnrecoverable:  true,
		},
		CMAF:        cmaf,
		Packets:     pkts,
		FECBlocks:   fec,
		DamagedCMAF: damaged,
	}
}

// ticksToWrapMargin converts ~2 media-seconds of wrap headroom into ticks.
func ticksToWrapMargin(timescale uint32) uint64 {
	return uint64(timescale) * 2
}
