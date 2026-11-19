package fixture

import (
	"math"
	"math/rand"

	"mediamix/internal/core"
	"mediamix/internal/timeline"
)

// buildTrack generates one track's elementary stream, keyframe index, packets
// and audio material.
func (f *Fixture) buildTrack(spec TrackSpec, rng *rand.Rand) error {
	if spec.Kind == timeline.KindAudio {
		return f.buildAudio(spec, rng)
	}
	return f.buildVideo(spec, rng)
}

// buildVideo encodes a synthetic Annex-B stream: I-frame every GOP, P-frames
// between. Each frame is a real start code plus a minimal header so codec
// probes can classify it.
func (f *Fixture) buildVideo(spec TrackSpec, rng *rand.Rand) error {
	if spec.Format != "h264" && spec.Format != "h265" && spec.Format != "av1" {
		return &core.Error{Code: core.CodeBadArgument, Track: spec.ID, Detail: "unsupported fixture codec", Fatal: true}
	}
	frames := spec.PacketCount
	gop := 15
	if frames < gop {
		gop = frames
	}
	if gop < 1 {
		return &core.Error{Code: core.CodeEmptyTrack, Track: spec.ID, Detail: "video track has no frames", Fatal: true}
	}
	var stream []byte
	var idx []Keyframe
	var packets []SentPacket
	stepUs := spec.DurationUs / timeline.Micros(frames)
	for i := 0; i < frames; i++ {
		key := i%gop == 0
		payload := encodeFrame(spec.Format, key, rng)
		off := len(stream)
		stream = append(stream, payload...)
		pts := timeline.Micros(i)*stepUs + spec.Origin
		if key {
			idx = append(idx, Keyframe{OffsetUs: timeline.Micros(off), Size: len(payload), PTS: pts, Key: true})
		}
		ts, err := ticksFor(spec.Timescale, pts)
		if err != nil {
			return err
		}
		packets = append(packets, SentPacket{
			Track: spec.ID, Seq: uint16(i), Timestamp: ts, Marker: true, Keyframe: key,
			Payload: payload, SentUs: pts,
		})
	}
	f.Streams[spec.ID] = stream
	f.Indexes[spec.ID] = idx
	f.Sent[spec.ID] = packets
	return nil
}

// buildAudio generates PCM and a packet timeline for the track.
func (f *Fixture) buildAudio(spec TrackSpec, rng *rand.Rand) error {
	a := spec.Audio
	if a == nil {
		return &core.Error{Code: core.CodeBadArgument, Track: spec.ID, Detail: "audio track without audio spec", Fatal: true}
	}
	ch := 1
	if a.Layout == "stereo" {
		ch = 2
	}
	secs := float64(spec.DurationUs) / float64(timeline.MicrosPerSecond)
	frames := int(math.Round(secs * float64(a.Rate)))
	samples := make([]float64, frames*ch)
	if !a.Silent {
		for i := 0; i < frames; i++ {
			v := a.Amplitude * math.Sin(2*math.Pi*a.FreqHz*float64(i)/float64(a.Rate))
			for c := 0; c < ch; c++ {
				samples[i*ch+c] = v
			}
		}
	}
	span := timeline.Span{Start: spec.Origin, End: spec.Origin + spec.DurationUs}
	f.Audio[spec.ID] = &Material{Track: spec.ID, Rate: a.Rate, Layout: a.Layout, Span: span, Samples: samples}

	var packets []SentPacket
	for i := 0; i < spec.PacketCount; i++ {
		at := spec.Origin + timeline.Micros(i)*spec.SegmentLen
		// Payload bytes stand in for coded audio; the digest only needs
		// deterministic content, not decodability.
		buf := make([]byte, 24)
		rng.Read(buf)
		ts, err := ticksFor(spec.Timescale, at)
		if err != nil {
			return err
		}
		packets = append(packets, SentPacket{
			Track: spec.ID, Seq: uint16(i), Timestamp: ts, Marker: true,
			Keyframe: i%30 == 0, Payload: buf, SentUs: at,
		})
	}
	f.Sent[spec.ID] = packets
	return nil
}

// encodeFrame synthesizes a minimal frame that passes the codec probe.
func encodeFrame(format string, key bool, rng *rand.Rand) []byte {
	body := make([]byte, 48)
	rng.Read(body)
	switch format {
	case "h264":
		prefix := []byte{0, 0, 0, 1}
		hdr := byte(0x41) // non-IDR slice, ref idc 2
		if key {
			hdr = 0x65 // IDR
		}
		return append(append(prefix, hdr), body...)
	case "h265":
		prefix := []byte{0, 0, 0, 1}
		var hdr byte
		if key {
			hdr = 19 << 1 // IDR_W_RADL
		} else {
			hdr = 1 << 1 // TRAIL_R
		}
		return append(append(prefix, hdr), body...)
	case "av1":
		// temporal delimiter, sequence header (if key), frame OBU.
		var out []byte
		out = append(out, obu(2, []byte{0x00})...)
		if key {
			out = append(out, obu(1, []byte{0x0a, 0x0b})...)
		}
		frameType := byte(1) // INTER
		if key {
			frameType = 0
		}
		hdr := byte(0x00)
		// show_existing_frame = 0, frame_type in bits 6..5, rest zero.
		hdr = frameType << 5
		out = append(out, obu(6, append([]byte{hdr}, body...))...)
		return out
	}
	return body
}

// obu builds an AV1 OBU with the size field present.
func obu(typ byte, payload []byte) []byte {
	hdr := byte(0x02) | (typ << 3) // has_size = 1
	size := leb(byte(len(payload)))
	return append(append([]byte{hdr}, size...), payload...)
}

// leb encodes a small LEB128 value (payloads here always fit in 7 bits).
func leb(v byte) []byte {
	if v&0x80 == 0 {
		return []byte{v}
	}
	return []byte{v&0x7f | 0x80, v >> 7}
}

// ticksFor converts a relative duration to ticks at a timescale.
func ticksFor(timescale uint32, us timeline.Micros) (uint32, error) {
	if timescale == 0 {
		return 0, &core.Error{Code: core.CodeBadTimestamp, Detail: "fixture timescale is zero", Fatal: true}
	}
	if us < 0 {
		return 0, &core.Error{Code: core.CodeBadTimestamp, Detail: "fixture timestamp is negative", Fatal: true}
	}
	t := (int64(us)*int64(timescale) + int64(timeline.MicrosPerSecond)/2) / int64(timeline.MicrosPerSecond)
	return uint32(t), nil
}
