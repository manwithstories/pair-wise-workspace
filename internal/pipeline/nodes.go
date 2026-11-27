package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"relayctl/internal/checksum"
	"relayctl/internal/cmaf"
	"relayctl/internal/clock"
	"relayctl/internal/dag"
	"relayctl/internal/sync"
	"relayctl/internal/transport"

	"github.com/pion/rtp"
)

const ssrc = uint32(0x11110000)

// makePacket rebuilds a media RTP packet for a surviving sequence number.
// Payload contents are not needed by XOR recovery beyond parity, but real
// packets keep the transport package's pion types honest.
func makePacket(seq uint16) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SSRC: ssrc, SequenceNumber: seq, Marker: true},
		Payload: []byte{byte(seq), byte(seq >> 8), 0, 0},
	}
}

// inputDigest hashes the fixture identity so every node key is stable.
func (e *Engine) inputDigest() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%t", e.scenario.Manifest.Case, e.scenario.Manifest.Seed, e.cfg.Damaged)
	return hex.EncodeToString(h.Sum(nil))
}

// runRecover applies local XOR FEC. A single hole per group is rebuilt; a
// burst leaves explicit unrecoverable sequence positions.
func (e *Engine) runRecover(_ *dag.Context) (string, error) {
	plan := e.scenario.Manifest.SequencePlan
	lostSet := map[uint16]bool{}
	for _, s := range plan.SingleLosses {
		lostSet[s] = true
	}
	for _, s := range plan.BurstLosses {
		lostSet[s] = true
	}

	// Present media packets = every sequence except declared losses.
	present := map[uint16]*rtp.Packet{}
	var allSeqs []uint16
	for _, g := range plan.FECGroups {
		allSeqs = append(allSeqs, g...)
	}
	for _, seq := range allSeqs {
		if !lostSet[seq] {
			present[seq] = makePacket(seq)
		}
	}

	var missing []uint16
	for _, seq := range allSeqs {
		if lostSet[seq] {
			missing = append(missing, seq)
		}
	}

	blocks := e.buildFECBlocks()
	recovered, unrecoverable, err := transport.Recover(present, missing, blocks)
	st := recoveryState{Recovered: len(recovered), Present: map[uint16]bool{}}
	for seq := range present {
		st.Present[seq] = true
	}
	for seq := range recovered {
		st.Present[seq] = true
	}
	st.Unrecoverable = unrecoverable
	e.recovery = st
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnrecoverable, err)
	}
	return fmt.Sprintf("recovered=%d", st.Recovered), nil
}

// buildFECBlocks converts fixture FEC specs into transport blocks.
func (e *Engine) buildFECBlocks() []transport.FECBlock {
	plan := e.scenario.Manifest.SequencePlan
	blocks := make([]transport.FECBlock, 0, len(plan.FECGroups))
	for i, group := range plan.FECGroups {
		if len(group) == 0 {
			continue
		}
		parity := e.parityFor(i)
		blocks = append(blocks, transport.FECBlock{
			FirstSeq: group[0], LastSeq: group[len(group)-1], Parity: parity,
		})
	}
	return blocks
}

// parityFor recomputes XOR parity from surviving media bytes; the fixture
// does not persist parity payloads on reload, so derive them deterministically.
func (e *Engine) parityFor(groupIdx int) []byte {
	group := e.scenario.Manifest.SequencePlan.FECGroups[groupIdx]
	var pkts []*rtp.Packet
	for _, seq := range group {
		pkts = append(pkts, makePacket(seq))
	}
	blk, _ := transport.EncodeFEC(pkts)
	return blk.Parity
}

// runParse validates the recovered fragment stream structurally.
func (e *Engine) runParse(_ *dag.Context) (string, error) {
	frags, off, err := e.parseFragments()
	if err != nil {
		e.damageOff = off
		return "", err
	}
	e.fragments = frags
	return fmt.Sprintf("fragments=%d", len(frags)), nil
}

// runSync feeds every track through the shared clock and aligns A/V.
func (e *Engine) runSync(_ *dag.Context) (string, error) {
	tracks := map[uint32]cmaf.TrackFragment{}
	for _, f := range e.fragments {
		for _, tf := range f.Tracks {
			tracks[tf.TrackID] = tf
		}
	}
	timelines := map[uint32]sync.TrackTimeline{}
	var worst clock.Confidence
	for id, tf := range tracks {
		obs, err := e.mc.ObserveTfdt(id, tf.BaseMediaDecodeTime)
		if err != nil {
			worst = clock.Rejected
		}
		if obs.Confidence > worst {
			worst = obs.Confidence
		}
		durations := make([]uint32, len(tf.Samples))
		for i, s := range tf.Samples {
			durations[i] = s.Duration
		}
		points, perr := e.mc.ProjectSamples(id, tf.BaseMediaDecodeTime, durations)
		if perr != nil {
			worst = clock.Rejected
		}
		kind := "video"
		if id == 2 {
			kind = "audio"
		}
		timelines[id] = sync.TrackTimeline{
			TrackID: id, Kind: kind, Points: points, ClockConfidence: worst,
		}
	}
	aligner := sync.New(e.mc)
	res, err := aligner.Align(timelines[2], timelines[1])
	e.syncResult = res
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("offset_ms=%d confidence=%s", res.WorstAbsMS, res.Confidence), nil
}

// runPack emits normalized fragment artifacts into the state output dir.
func (e *Engine) runPack(_ *dag.Context) (string, error) {
	out := filepath.Join(e.cfg.StateDir, "out")
	if err := writeArtifacts(out, e); err != nil {
		return "", err
	}
	return out, nil
}

// runVerify seals and checks the checksum manifest over the artifacts.
func (e *Engine) runVerify(_ *dag.Context) (string, error) {
	out := filepath.Join(e.cfg.StateDir, "out")
	m, err := checksum.Build(out, outManifest)
	if err != nil {
		return "", err
	}
	if _, err := checksum.Write(out, outManifest, m); err != nil {
		return "", err
	}
	if err := checksum.Verify(out, outManifest); err != nil {
		return "", err
	}
	e.manifestDigest = m.ManifestDigest
	return m.ManifestDigest, nil
}
