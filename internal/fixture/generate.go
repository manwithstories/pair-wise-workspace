package fixture

import (
	"fmt"
	"math/rand"

	"github.com/pairwise/mediapack/internal/audio"
	"github.com/pairwise/mediapack/internal/demux"
	"github.com/pairwise/mediapack/internal/ingest"
	"github.com/pairwise/mediapack/internal/timeline"
	"github.com/pairwise/mediapack/internal/transport"
)

// Generate builds the deterministic verification sample.
func Generate(cfg Config) (*Sample, error) {
	if cfg.Seed == 0 {
		cfg.Seed = 73
	}
	faults := faultSet(cfg.Faults)
	rng := rand.New(rand.NewSource(cfg.Seed))
	b := NewTSBuilder()

	wrapPCR := faults["pcr-wrap"]
	wrapBase := uint64(0)
	if wrapPCR {
		// Half a second before the 33-bit wrap; the 640 ms sample crosses it.
		wrapBase = timeline.PCRModulus - timeline.PCRHz/2
	}
	rawTicks := func(mediaTicks uint64) uint64 {
		if !wrapPCR {
			return mediaTicks
		}
		return (wrapBase + mediaTicks) % timeline.PCRModulus
	}

	// PAT v1, two consistent copies.
	pat := transport.BuildPSIPayload(0x00, ProgramNumber, 1,
		transport.BuildPATEntries(ProgramNumber, PMTPID))
	for range 2 {
		if err := b.Append(0, true, pat, nil); err != nil {
			return nil, err
		}
	}

	psiUpdate := faults["psi-update"]
	initialStreams := []transport.PMTStream{{StreamType: transport.StreamTypeH264, PID: VideoPID, Codec: "h264-synthetic"}}
	if !psiUpdate {
		initialStreams = append(initialStreams, transport.PMTStream{
			StreamType: transport.StreamTypePCMS16LE, PID: AudioPID, Codec: "pcm_s16le",
		})
	}
	pmt1 := transport.BuildPSIPayload(0x02, ProgramNumber, 1,
		transport.BuildPMTEntries(VideoPID, initialStreams))
	for range 2 {
		if err := b.Append(PMTPID, true, pmt1, nil); err != nil {
			return nil, err
		}
	}

	bodies := make([][]byte, Clips)
	for i := range Clips {
		if i == Clips-1 {
			// Reuse clip one's exact elementary body to create provenance duplicate.
			bodies[i] = append([]byte(nil), bodies[0]...)
			continue
		}
		body := make([]byte, 64)
		if _, err := rng.Read(body); err != nil {
			return nil, err
		}
		bodies[i] = body
	}

	for clip := range Clips {
		clipBase := uint64(clip) * ClipTick

		if clip == 1 && psiUpdate {
			streams2 := []transport.PMTStream{
				{StreamType: transport.StreamTypeH264, PID: VideoPID, Codec: "h264-synthetic"},
				{StreamType: transport.StreamTypePCMS16LE, PID: AudioPID, Codec: "pcm_s16le"},
			}
			pmt2 := transport.BuildPSIPayload(0x02, ProgramNumber, 2,
				transport.BuildPMTEntries(VideoPID, streams2))
			if faults["bad-crc"] {
				bad := append([]byte(nil), pmt2...)
				bad[len(bad)-1] ^= 0xff // corrupt final CRC byte
				if err := b.Append(PMTPID, true, bad, nil); err != nil {
					return nil, err
				}
			}
			for range 2 {
				if err := b.Append(PMTPID, true, pmt2, nil); err != nil {
					return nil, err
				}
			}
		}

		// Audio shares the clip's first PTS. Write it before later video frames
		// so assembly order is non-decreasing on the shared program PTS clock.
		// With PSI update, audio becomes known at clip 1; without it PMT v1
		// already advertised PCM and all clips carry audio.
		if !psiUpdate || clip >= 1 {
			pcm := generatePCM()
			audioMedia := demux.BuildMediaFrame(100+uint32(clip), false, pcm)
			if err := b.AppendPES(AudioPID, 0xc0, rawTicks(clipBase), audioMedia, nil); err != nil {
				return nil, err
			}
		}

		for frame := range FramesPerClip {
			mediaTick := clipBase + uint64(frame)*FrameTick
			seq := uint32(clip*FramesPerClip + frame + 1)
			key := frame == 0
			media := demux.BuildMediaFrame(seq, key, bodies[clip])
			pts := rawTicks(mediaTick)
			pcr := rawTicks(mediaTick)

			if clip == 1 && frame == 0 && faults["cc-gap"] {
				// Four prior video packets used CC 0..3 and the builder's next
				// CC is 4. Skip to 5 so the reader reports missing CC 4.
				b.cc[VideoPID] = 5
			}
			if err := b.AppendPES(VideoPID, 0xe0, pts, media, &pcr); err != nil {
				return nil, err
			}
		}
	}

	complete := append([]byte(nil), b.Bytes()...)
	packetizer := &ingest.RTPPacketizer{SSRC: uint32(cfg.Seed), Sequence: 0}
	rtpPackets, err := packetizer.PacketizeTS(complete)
	if err != nil {
		return nil, fmt.Errorf("packetize fixture RTP: %w", err)
	}
	datagrams, err := ingest.MarshalRTP(rtpPackets)
	if err != nil {
		return nil, err
	}

	fullTS := complete
	if faults["truncated"] {
		tail := make([]byte, 100) // deliberately shorter than one 188-byte packet
		tail[0] = transport.SyncByte
		fullTS = append(append([]byte(nil), complete...), tail...)
	}

	return &Sample{
		Seed: cfg.Seed, Faults: faults, CompleteTS: complete, TS: fullTS,
		RTPDatagrams: datagrams, PacketCount: len(complete) / transport.PacketSize,
		ExpectedClips: Clips,
	}, nil
}

func generatePCM() []byte {
	const windowSamples = SampleRate * 20 / 1000 // 160
	pcm := make([]byte, 0, 8*windowSamples*2)
	for w := 0; w < 8; w++ {
		amplitude := int16(9000)
		if w >= 1 && w <= 5 { // five windows = 100 ms of digital silence
			amplitude = 0
		}
		pcm = append(pcm, audio.TonePCM(windowSamples, amplitude)...)
	}
	return pcm
}
