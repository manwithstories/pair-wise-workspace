package source

import (
	"bytes"
	"fmt"
)

// Generate builds a complete length-prefixed RTP/RTCP fixture stream.
func Generate(spec Spec) ([]byte, error) {
	if spec.PacketCount <= 0 || spec.FramesPerPacket <= 0 {
		return nil, fmt.Errorf("source: invalid fixture size: %+v", spec)
	}
	totalFrames := spec.PacketCount * spec.FramesPerPacket
	pcm := ToneL16(totalFrames, spec.Channels, spec.SampleRate, 0.18, 997)
	payloadBytes := spec.FramesPerPacket * spec.Channels * 2

	datagrams := make(map[int][]byte, spec.PacketCount)
	for seq := 0; seq < spec.PacketCount; seq++ {
		start := seq * payloadBytes
		payload := pcm[start : start+payloadBytes]
		ts := uint32(seq * spec.FramesPerPacket)
		d, err := RTPDatagram(spec.SSRC, spec.PayloadType, uint16(seq), ts, payload)
		if err != nil {
			return nil, fmt.Errorf("marshal seq %d: %w", seq, err)
		}
		datagrams[seq] = d
	}

	var out bytes.Buffer
	ntpBase := uint32(3900000000)
	out.Write(FramePrefix(SenderReport(spec.SSRC, 0, ntpBase)))
	// A second SR one NTP second later makes the linear drift estimator
	// traceable end-to-end. The fixture clock is exact, so drift is 0 ppm.
	out.Write(FramePrefix(SenderReport(spec.SSRC, uint32(spec.SampleRate), ntpBase+1)))
	if spec.Case == "damaged" {
		out.Write(FramePrefix(BadCSRCDatagram()))
	}

	dropped := make(map[int]bool, len(spec.DroppedSeqs))
	for _, s := range spec.DroppedSeqs {
		dropped[s] = true
	}
	withheldEarly := false
	withheldExpired := false

	writePacket := func(seq int) {
		out.Write(FramePrefix(datagrams[seq]))
	}

	for seq := 0; seq < spec.PacketCount; seq++ {
		if dropped[seq] {
			continue
		}
		if spec.Case == "damaged" && seq == spec.EarlyLateSeq && !withheldEarly {
			withheldEarly = true
			continue
		}
		if spec.Case == "damaged" && seq == spec.ExpiredLateSeq && !withheldExpired {
			withheldExpired = true
			continue
		}
		if spec.SwapA != 0 && seq == spec.SwapA {
			writePacket(spec.SwapB)
			writePacket(spec.SwapA)
			if spec.DuplicateSeq == spec.SwapA {
				writePacket(spec.SwapA)
			}
			continue
		}
		if spec.SwapB != 0 && seq == spec.SwapB {
			continue
		}

		writePacket(seq)
		if spec.DuplicateSeq == seq {
			writePacket(seq)
		}
		if spec.Case == "damaged" && seq == 30 {
			out.Write(FramePrefix(BadExtensionDatagram()))
		}
		if spec.Case == "damaged" && seq == 49 && withheldEarly {
			writePacket(spec.EarlyLateSeq)
		}
	}

	if spec.Case == "damaged" && withheldExpired {
		writePacket(spec.ExpiredLateSeq)
	}
	return out.Bytes(), nil
}
