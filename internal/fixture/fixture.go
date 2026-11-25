// Package fixture deterministically generates all media samples and injected
// faults used by tests and the terminal demonstration.
package fixture

import (
	"encoding/binary"
	"math"
	"time"

	"github.com/pion/rtp"

	"mediacheck/internal/ingest"
	"mediacheck/internal/media"
	"mediacheck/internal/txn"
)

const (
	FrameDurationTicks    int64 = 3000
	FrameCount            int   = 60
	KeyframeInterval      int   = 30
	KeyframeIntervalTicks int64 = FrameDurationTicks * int64(KeyframeInterval)
	VideoTargetBPS        int64 = 1_000_000
	ContainerTargetBPS    int64 = 2_550_000
	PCMSampleRate               = 44100
	PCMChannels                 = 1
)

// Bundle is one complete coherent supplier sample.
type Bundle struct {
	Video      []byte
	Codec      ingest.Codec
	Clock      *media.ClockDeclaration
	FrameDur   int64
	PCM        []byte
	PCMFormat  ingest.PCMFormat
	Index      *ingest.KeyframeIndex
	RTPPackets []rtp.Packet
}

// Normal builds deterministic H.264 Annex-B video and 44.1 kHz mono PCM.
func Normal() *Bundle {
	clock := mediaClock()
	video, entries, packets := buildH264()
	pcm := buildPCM()
	idx := &ingest.KeyframeIndex{
		Codec: string(ingest.CodecH264), Clock: clock,
		SourceSize: int64(len(video)), SourceSHA256: txn.SHA256Bytes(video),
		GeneratedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		// Coherent fixture remains valid for deterministic test clocks.
		ExpiresAt:             time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
		KeyframeIntervalTicks: KeyframeIntervalTicks,
		Entries:               entries,
	}
	return &Bundle{
		Video: video, Codec: ingest.CodecH264, Clock: &clock, FrameDur: FrameDurationTicks,
		PCM: pcm, PCMFormat: ingest.PCMFormat{SampleRate: PCMSampleRate, Channels: PCMChannels},
		Index: idx, RTPPackets: packets,
	}
}

func mediaClock() media.ClockDeclaration {
	return media.DefaultClock()
}

func buildH264() ([]byte, []ingest.IndexEntry, []rtp.Packet) {
	var buf []byte
	var entries []ingest.IndexEntry
	var packets []rtp.Packet
	// 4147-byte NAL payload makes a 30-frame segment almost exactly 125000 B.
	const nalPayloadSize = 4147
	for i := 0; i < FrameCount; i++ {
		key := i%KeyframeInterval == 0
		payload := make([]byte, nalPayloadSize)
		if key {
			payload[0] = 0x65 // IDR slice
		} else {
			payload[0] = 0x41 // non-IDR slice
		}
		for j := 1; j < len(payload); j++ {
			payload[j] = 0x5a // no accidental Annex-B start-code zeros
		}
		offset := int64(len(buf))
		buf = append(buf, 0x00, 0x00, 0x00, 0x01)
		buf = append(buf, payload...)
		pts := int64(i) * FrameDurationTicks
		if key {
			seq := uint16(100 + len(entries))
			ts := uint32(pts)
			entries = append(entries, ingest.IndexEntry{
				PTS: pts, Offset: offset, Size: 4 + len(payload), Keyframe: true,
				RTPSequence: seq, RTPTimestamp: ts,
			})
			packets = append(packets, rtp.Packet{
				Header: rtp.Header{
					Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: ts, SSRC: 0x4d43484b,
				},
				Payload: payload[:16],
			})
		}
	}
	return buf, entries, packets
}

func buildPCM() []byte {
	frames := 2 * PCMSampleRate
	out := make([]byte, frames*2*PCMChannels)
	for i := 0; i < frames; i++ {
		// Moderate amplitude: loudness normalization has ample peak headroom.
		v := int16(0.25 * 32767 * math.Sin(2*math.Pi*440*float64(i)/float64(PCMSampleRate)))
		binary.LittleEndian.PutUint16(out[2*i:], uint16(v))
	}
	return out
}

// TruncatedH264 returns an Annex-B stream whose final NAL has only a header.
func TruncatedH264() []byte {
	b := []byte{0x00, 0x00, 0x00, 0x01, 0x65}
	payload := make([]byte, 128)
	for i := range payload {
		payload[i] = 0x5a
	}
	b = append(b, payload...)
	b = append(b, 0x00, 0x00, 0x00, 0x01, 0x65) // truncated final IDR
	return b
}

// TruncatedAV1 returns an OBU whose LEB128 size exceeds the supplied bytes.
func TruncatedAV1() []byte {
	// type=6 (frame), size-present bit set; declared size=200.
	return []byte{0x32, 0xc8, 0x01, 0x10, 0x20, 0x30}
}

// WrapRTPIndex builds an otherwise valid index with a 32-bit RTP timestamp wrap.
func WrapRTPIndex() (*ingest.KeyframeIndex, []rtp.Packet) {
	clock := mediaClock()
	const first = math.MaxUint32 - 9000
	const wrapped = uint32(80999) // first + 90000 modulo 2^32
	idx := &ingest.KeyframeIndex{
		Codec: string(ingest.CodecH264), Clock: clock, KeyframeIntervalTicks: 90000,
		Entries: []ingest.IndexEntry{
			{PTS: 0, Offset: 4096, Size: 100, Keyframe: true, RTPSequence: 400, RTPTimestamp: first},
			{PTS: 90000, Offset: 131072, Size: 100, Keyframe: true, RTPSequence: 401, RTPTimestamp: wrapped},
		},
	}
	pkts := []rtp.Packet{
		{Header: rtp.Header{Version: 2, SequenceNumber: 400, Timestamp: first}},
		{Header: rtp.Header{Version: 2, SequenceNumber: 401, Timestamp: wrapped}},
	}
	return idx, pkts
}
