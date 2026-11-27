// Package fixture synthesises the local, deterministic sample inputs used by
// the ingestcheck demo. Nothing is read from the network and no transcoding
// is performed: the samples are built byte-for-byte in process.
package fixture

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"

	"github.com/pion/rtp"

	"pairwise/ingestcheck/internal/mp4"
	"pairwise/ingestcheck/internal/pipeline"
	"pairwise/ingestcheck/internal/track"
)

// Sample constants mirror a short CMAF pair: six one-second fragments per
// track. Video runs at 60 Hz on a 90 kHz clock, audio at 50 frames per
// second on a 48 kHz clock, and audio starts 10 ms after video.
const (
	VideoFragments = 6
	AudioFragments = 6
	VideoTrackID   = 1
	AudioTrackID   = 2

	VideoFrameTick = 1500 // 90 kHz, 60 Hz
	AudioFrameTick = 960  // 48 kHz, 20 ms
	VideoFrames    = 60
	AudioFrames    = 50

	AudioOffsetTicks = 480 // 10 ms at 48 kHz: a realistic small A/V offset
)

// Fragment is one generated sample: its moof+mdat bytes, its first packet
// sequence number and the RTP datagrams that carry its media.
type Fragment struct {
	TrackID uint32
	Seq     uint32
	Data    []byte
	Packets [][]byte
	Missing int
	BW      float64
}

// VideoTimeline returns the base decode time of the healthy video fragment
// with ordinal i (one second apart).
func VideoTimeline(i int) uint64 { return uint64(i) * VideoFrames * VideoFrameTick }

// AudioTimeline returns the base decode time of the healthy audio fragment
// with ordinal i, offset by 10 ms from video.
func AudioTimeline(i int) uint64 {
	return uint64(i)*AudioFrames*AudioFrameTick + AudioOffsetTicks
}

// BWF returns a smoothly varying bandwidth sample for window i. Every third
// window dips low so the EWMA and the recovery budget see a changing link.
func BWF(i int) float64 {
	base := 8.0
	if i%3 == 0 {
		base = 2.0
	}
	return base + float64(i%4)*0.5
}

// build assembles one fragment from a spec. ord selects the media content,
// while seq and base control the fragment header so scenarios can rewrite
// them independently.
func build(trackID uint32, seq uint32, base uint64, ord, frames, frameTick int, allKey bool, sizeBase int) Fragment {
	samples := make([]mp4.Sample, 0, frames)
	for f := 0; f < frames; f++ {
		key := allKey || f == 0
		samples = append(samples, mp4.Sample{
			Duration:        uint32(frameTick),
			Size:            sampleSize(trackID, ord, f, sizeBase),
			Flags:           mp4.SampleFlagsFor(key),
			CompositionTime: 0,
		})
	}
	media := payload(trackID, ord, frames)
	const fileFlags = 0x000001 | 0x000100 | 0x000200 | 0x000400
	data := mp4.Assemble(seq, base, fileFlags, samples, media)
	return Fragment{
		TrackID: trackID,
		Seq:     seq,
		Data:    data,
		Packets: packets(trackID, packetBase(trackID, ord), media),
		BW:      BWF(ord),
	}
}

// packetsPerFragment is the packet count for one fragment of each track.
// Video carries 60 samples of 8 bytes, split into 64-byte chunks; audio
// carries 50 samples, likewise split.
const (
	videoPacketsPerFragment = 8
	audioPacketsPerFragment = 7
)

// packetBase returns the first RTP sequence number for a track's fragment.
// Consecutive fragments continue the sequence so the window sees a single
// monotonic stream per track rather than one that jumps at every boundary.
func packetBase(track uint32, ord int) uint16 {
	if track == VideoTrackID {
		return uint16(16 + ord*videoPacketsPerFragment)
	}
	return uint16(16 + VideoFragments*videoPacketsPerFragment + ord*audioPacketsPerFragment)
}

func buildVideo(seq uint32, base uint64, ord int) Fragment {
	return build(VideoTrackID, seq, base, ord, VideoFrames, VideoFrameTick, false, 480)
}

func buildAudio(seq uint32, base uint64, ord int) Fragment {
	return build(AudioTrackID, seq, base, ord, AudioFrames, AudioFrameTick, true, 160)
}

func demoGraph() (*track.Graph, error) { return track.DemoGraph() }

func sampleSize(track uint32, frag, idx, base int) uint32 {
	// Deterministic, content-dependent size so hashes differ per segment.
	return uint32(base + (int(track)*7+frag*3+idx*5)%31)
}

func payload(track uint32, frag, count int) []byte {
	if count <= 0 {
		count = 1
	}
	const per = 8
	out := make([]byte, 0, count*per)
	h := fnv.New64a()
	for i := 0; i < count; i++ {
		_, _ = h.Write([]byte(fmt.Sprintf("%d:%d:%d", track, frag, i)))
		v := h.Sum64()
		chunk := make([]byte, per)
		binary.BigEndian.PutUint64(chunk, v)
		out = append(out, chunk...)
	}
	return out
}

// packets splits media into RTP datagrams starting at base and stepping by
// one; payloads are the original byte chunks.
func packets(track uint32, base uint16, media []byte) [][]byte {
	const chunk = 64
	var out [][]byte
	off, n := 0, 0
	for off < len(media) {
		end := off + chunk
		if end > len(media) {
			end = len(media)
		}
		p := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: base + uint16(n),
				Timestamp:      uint32(n),
				SSRC:           track,
				Marker:         end >= len(media),
			},
			Payload: append([]byte(nil), media[off:end]...),
		}
		buf, err := p.Marshal()
		if err != nil {
			panic(err)
		}
		out = append(out, buf)
		off = end
		n++
	}
	return out
}

// MarshalPacket builds a minimal datagram with the given sequence and SSRC;
// tests use it to inject malformed or out-of-order packets.
func MarshalPacket(seq uint16, ssrc uint32) []byte {
	p := &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: 0, SSRC: ssrc},
		Payload: []byte{0x01, 0x02},
	}
	buf, err := p.Marshal()
	if err != nil {
		panic(err)
	}
	return buf
}

// toInput converts a generated fragment into a pipeline input.
func toInput(f Fragment) pipeline.FragmentInput {
	return pipeline.FragmentInput{
		TrackID:   f.TrackID,
		Seq:       f.Seq,
		Data:      f.Data,
		Packets:   f.Packets,
		Missing:   f.Missing,
		Bandwidth: f.BW,
	}
}

func graphOrPanic() *track.Graph {
	g, err := demoGraph()
	if err != nil {
		panic(err)
	}
	return g
}
