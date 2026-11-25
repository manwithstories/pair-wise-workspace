package demo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/pion/rtp"

	"fieldpost/demux"
	"fieldpost/fixture"
	"fieldpost/index"
	"fieldpost/internal/core"
	"fieldpost/mapping"
	"fieldpost/remux"
	"fieldpost/transport"
)

// HealthyResult is one verified 3-camera shard.
type HealthyResult struct {
	Camera     string
	ID         string
	Format     string
	Hash       string
	KeyHits    int
	KeyTotal   int
	OutputPath string
	Retries    int
	Duplicates int
	Reordered  int
}

func processHealthy(_ context.Context, seed int64, camera string, idx int, outDir string) (*HealthyResult, error) {
	canonical := fixture.MediaSegment(seed, camera, idx)
	format := fixture.Formats[camera]
	wire, err := sourceContainer(canonical, format)
	if err != nil {
		return nil, err
	}

	base := uint16(65519 + idx*19 + len(camera)*7)
	packets := fixture.RTPPacketize(canonical.ID, base, wire, 160)
	if len(packets) < 4 {
		return nil, core.InvariantError(core.StageFixture, canonical.ID, 0, "too few RTP packets")
	}
	bySeq := make(map[uint16]*rtp.Packet, len(packets))
	for _, p := range packets {
		bySeq[p.SequenceNumber] = p
	}
	missing := packets[len(packets)/2].SequenceNumber
	incoming := impairPackets(seed, packets, len(packets)/2)
	attempts := map[uint16]int{}
	fetch := func(_ context.Context, seq uint16) (*rtp.Packet, error) {
		attempts[seq]++
		if seq == missing && attempts[seq] == 1 {
			return nil, errors.New("simulated busy local source")
		}
		p, ok := bySeq[seq]
		if !ok {
			return nil, errors.New("packet absent from local source")
		}
		return p, nil
	}
	ordered, rp, err := transport.RecoverSegment(context.Background(), canonical.ID, base,
		len(packets), incoming, fetch, 128)
	if err != nil {
		return nil, err
	}
	recoveredWire := fixture.ReassembleRTP(ordered)
	if !bytes.Equal(recoveredWire, wire) {
		return nil, core.InvariantError(core.StageTransport, canonical.ID, int64(idx),
			"recovered container byte mismatch")
	}

	parsed, err := parseSource(canonical.ID, format, fmt.Sprintf("%s-%d", camera, idx), recoveredWire)
	if err != nil {
		return nil, err
	}
	mp, err := mapping.DefaultRules().Map(camera, parsed.Tracks)
	if err != nil {
		return nil, err
	}
	mappedSamples := mp.RemapSamples(parsed.Samples)
	if err := core.VerifyEssence(core.StageInvariant, canonical.ID, canonical.Samples, mappedSamples); err != nil {
		return nil, err
	}
	outputTracks := []core.Track{mp.Video, mp.Audio}
	outputTracks[0].ID = mp.OutputVideo
	outputTracks[1].ID = mp.OutputAudio

	out, err := remux.CMAF(outputTracks, mappedSamples)
	if err != nil {
		return nil, err
	}
	reparsed, err := demux.ParseMP4(canonical.ID+".cmaf", canonical.ID+".m4s", out.Data)
	if err != nil {
		return nil, err
	}
	if err := core.VerifyEssence(core.StageInvariant, canonical.ID+".m4s", canonical.Samples, reparsed.Samples); err != nil {
		return nil, err
	}
	if _, err := demux.GoMP4TopLevel(out.Data); err != nil {
		return nil, core.Wrap(core.StageInvariant, core.ExitInvariant, false, err, "go-mp4 header check")
	}
	ki := index.Build(reparsed)
	hits, total := ki.VerifyExact()
	path, err := remux.AtomicWrite(outDir, canonical.ID+".m4s", out.Data)
	if err != nil {
		return nil, err
	}
	return &HealthyResult{
		Camera: camera, ID: canonical.ID, Format: format, Hash: core.EssenceHash(canonical),
		KeyHits: hits, KeyTotal: total, OutputPath: path, Retries: rp.Retries,
		Duplicates: rp.Duplicates, Reordered: rp.Reordered,
	}, nil
}

func sourceContainer(seg *core.Segment, format string) ([]byte, error) {
	if format == "ts" {
		return demux.MuxTS(seg)
	}
	out, err := remux.CMAF(seg.Tracks, seg.Samples)
	if err != nil {
		return nil, err
	}
	return out.Data, nil
}

func parseSource(id, format, source string, data []byte) (*core.Segment, error) {
	if format == "ts" {
		return demux.ParseTS(id, source+".ts", data)
	}
	return demux.ParseMP4(id, source+".m4s", data)
}

func runUnrecoverableBurst(seed int64, outDir string) error {
	seg := fixture.MediaSegment(seed, "roving", 77)
	seg.ID = "unrecoverable-burst"
	wire, err := sourceContainer(seg, "mp4")
	if err != nil {
		return err
	}
	base := uint16(65530)
	packets := fixture.RTPPacketize(seg.ID, base, wire, 160)
	var incoming []*rtp.Packet
	for i, p := range packets {
		if i < 3 { // burst loss; local source has no copies
			continue
		}
		incoming = append(incoming, p)
	}
	fetch := func(context.Context, uint16) (*rtp.Packet, error) {
		return nil, errors.New("permanently missing local shard")
	}
	_, _, err = transport.RecoverSegment(context.Background(), seg.ID, base, len(packets), incoming, fetch, 128)
	if core.ExitCode(err) != core.ExitUnrecoverable {
		return core.InvariantError(core.StageInvariant, seg.ID, 0,
			"expected unrecoverable classification, got err=%v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(outDir, "*unrecoverable*"))
	if len(matches) != 0 {
		return core.InvariantError(core.StageRemux, seg.ID, 0,
			"unrecoverable segment left output: %v", matches)
	}
	return nil
}
