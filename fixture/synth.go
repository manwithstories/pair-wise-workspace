// Package fixture generates deterministic local media, manifests and network
// impairments. No external files or services are used.
package fixture

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math/rand"

	"github.com/pion/rtp"

	"fieldpost/internal/core"
)

// CameraNames are the three staged camera positions in the demo.
var CameraNames = []string{"stage", "side", "roving"}

// Formats maps each camera to its source container.
var Formats = map[string]string{
	"stage":  "ts",
	"side":   "mp4",
	"roving": "mp4",
}

// SegmentsPerCamera gives 3*4=12 healthy essence commitments.
const SegmentsPerCamera = 4

// MediaSegment creates one deterministic AVC/AAC segment. Timestamps start at
// a deliberately non-zero value so parsers cannot normalize the base to zero.
func MediaSegment(seed int64, camera string, index int) *core.Segment {
	h := fnv.New64a()
	_, _ = h.Write([]byte(camera))
	var seedBytes [8]byte
	binary.BigEndian.PutUint64(seedBytes[:], uint64(seed))
	_, _ = h.Write(seedBytes[:])
	_, _ = h.Write([]byte{byte(index), byte(index >> 8)})
	r := rand.New(rand.NewSource(int64(h.Sum64())))

	base := core.Ticks(900000 + int64(index+1)*90000 + int64(h.Sum64()%9000))
	seg := &core.Segment{
		ID:     fmt.Sprintf("%s-%02d", camera, index),
		Camera: camera,
		Index:  index,
		Tracks: []core.Track{
			{ID: 1, Kind: core.TrackVideo, Codec: "avc1", Timescale: 90000, Width: 1280, Height: 720},
			{ID: 2, Kind: core.TrackAudio, Codec: "mp4a", Timescale: 90000, Channels: 2},
		},
	}
	// Six video frames, IDR at frame zero and three. Interleave audio so each
	// three-frame group forms a clean keyframe fragment.
	for f := 0; f < 6; f++ {
		dts := base + core.Ticks(f)*3000
		key := f == 0 || f == 3
		seg.Samples = append(seg.Samples, core.Sample{
			TrackID: 1, PTS: dts, DTS: dts, Duration: 3000,
			Flags: videoFlags(key), Data: avcPayload(r, camera, index, f, key),
		})
		seg.Samples = append(seg.Samples, core.Sample{
			TrackID: 2, PTS: dts, DTS: dts, Duration: 3000,
			Flags: core.FlagSync, Data: aacPayload(r, camera, index, f),
		})
	}
	return seg
}

func videoFlags(key bool) uint32 {
	if key {
		return core.FlagSync
	}
	return core.FlagDepends
}

func avcPayload(r *rand.Rand, camera string, seg, frame int, key bool) []byte {
	out := make([]byte, 72)
	// Annex-B start code and NAL header: type 5 IDR, type 1 non-IDR.
	out[0], out[1], out[2], out[3] = 0, 0, 0, 1
	if key {
		out[4] = 0x65
	} else {
		out[4] = 0x41
	}
	out[5] = byte(seg)
	out[6] = byte(frame)
	out[7] = byte(len(camera))
	_, _ = r.Read(out[8:])
	return out
}

func aacPayload(r *rand.Rand, camera string, seg, frame int) []byte {
	out := make([]byte, 40)
	// ADTS sync and MPEG-4/Low-Complexity marker; payload remains synthetic.
	out[0] = 0xff
	out[1] = 0xf1
	out[2] = 0x50 | byte(seg&0x0f)
	out[3] = 0x80 | byte(frame&0x3f)<<2
	out[4] = byte(len(camera))
	_, _ = r.Read(out[5:])
	return out
}

// RTPPacketize splits a recovered container into bounded RTP payload chunks.
func RTPPacketize(segmentID string, baseSeq uint16, payload []byte, chunk int) []*rtp.Packet {
	if chunk <= 0 || chunk > 1200 {
		chunk = 180
	}
	var out []*rtp.Packet
	seq := baseSeq
	for len(payload) > 0 {
		n := minInt(chunk, len(payload))
		p := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: seq,
				Timestamp:      uint32(900000) + uint32(seq),
				SSRC:           0x4649454c, // "FIEL"
			},
			Payload: append([]byte(nil), payload[:n]...),
		}
		if len(payload) == n {
			p.Marker = true
		}
		out = append(out, p)
		payload = payload[n:]
		seq++
	}
	return out
}

// ReassembleRTP concatenates ordered recovered RTP payloads.
func ReassembleRTP(chunks [][]byte) []byte {
	var out []byte
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}

// BandwidthPoint is a deterministic bandwidth simulation sample.
type BandwidthPoint struct {
	AtMS      int
	BytesPerS int
}

// BandwidthProfile returns a sawtooth/step profile to exercise bounded cache.
func BandwidthProfile(seed int64) []BandwidthPoint {
	r := rand.New(rand.NewSource(seed ^ 0x5f3759df))
	out := make([]BandwidthPoint, 0, 16)
	bw := 2_000_000 + r.Intn(4_000_000)
	for i := 0; i < 16; i++ {
		bw += r.Intn(3_000_001) - 1_500_000
		if bw < 250_000 {
			bw = 250_000
		}
		if bw > 12_000_000 {
			bw = 12_000_000
		}
		out = append(out, BandwidthPoint{AtMS: i * 250, BytesPerS: bw})
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
