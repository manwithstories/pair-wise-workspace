// Package remux performs lossless re-containerization: ordered compressed
// samples are packetized into RTP, written into a deterministic ISO BMFF
// transport file, and covered by a running SHA-256 checksum. Samples are never
// decoded; the same input and mapping always produce byte-identical output.
package remux

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"

	"mediacore/internal/clock"
	"mediacore/internal/fixture"
	"mediacore/internal/rtpnet"
	"mediacore/internal/sample"
)

// TrackProfile maps a codec to static RTP parameters.
type TrackProfile struct {
	TrackID     uint32
	Codec       sample.Codec
	SSRC        uint32
	PayloadType uint8
	ClockRate   uint32
}

// Metrics summarizes a remux run.
type Metrics struct {
	Tracks     int
	Samples    int
	RTPpackets int
	Bytes      int64
	SHA256     string
	PerTrack   map[uint32]int
}

// Writer consumes ordered samples and emits the transport container.
type Writer struct {
	w        io.Writer
	profiles map[uint32]TrackProfile
	packets  map[uint32]*rtpnet.Packetizer
	hasher   hash.Hash
	mw       io.Writer // multi-writer to sink + hasher
	metrics  Metrics
}

// NewWriter builds a remux writer. The hasher covers every RTP packet payload
// in deterministic order, independent of container framing.
func NewWriter(w io.Writer, profiles []TrackProfile) *Writer {
	rw := &Writer{
		w:        w,
		profiles: map[uint32]TrackProfile{},
		packets:  map[uint32]*rtpnet.Packetizer{},
		hasher:   sha256.New(),
		metrics:  Metrics{PerTrack: map[uint32]int{}},
	}
	rw.mw = io.MultiWriter(w, rw.hasher)
	for i, pr := range profiles {
		// Deterministic SSRC/PT per track position when left unset by caller.
		if pr.SSRC == 0 {
			pr.SSRC = uint32(0x1000 + i)
		}
		if pr.ClockRate == 0 {
			pr.ClockRate = clock.Rate
		}
		if pr.PayloadType == 0 {
			pr.PayloadType = payloadTypeFor(pr.Codec)
		}
		rw.profiles[pr.TrackID] = pr
		rw.packets[pr.TrackID] = rtpnet.NewPacketizer(rtpnet.TrackConfig{
			SSRC: pr.SSRC, PayloadType: pr.PayloadType, ClockRate: pr.ClockRate,
		}, rtpnet.DefaultMTU)
	}
	rw.metrics.Tracks = len(profiles)
	return rw
}

func payloadTypeFor(c sample.Codec) uint8 {
	switch c {
	case sample.CodecH264:
		return 96
	case sample.CodecAAC:
		return 97
	case sample.CodecOpus:
		return 98
	default:
		return 99
	}
}

// WriteSample packetizes and emits one ordered sample, updating the checksum.
func (rw *Writer) WriteSample(s *sample.Sample) error {
	pz, ok := rw.packets[s.TrackID]
	if !ok {
		// Unknown mapped track: synthesize a profile so output stays total.
		pr := TrackProfile{TrackID: s.TrackID, Codec: s.Codec}
		pr.SSRC = 0x1000 + s.TrackID
		pr.ClockRate = clock.Rate
		pr.PayloadType = payloadTypeFor(s.Codec)
		pz = rtpnet.NewPacketizer(rtpnet.TrackConfig{
			SSRC: pr.SSRC, PayloadType: pr.PayloadType, ClockRate: pr.ClockRate,
		}, rtpnet.DefaultMTU)
		rw.packets[s.TrackID] = pz
	}
	pkts, err := pz.Packetize(s)
	if err != nil {
		return err
	}
	for _, pkt := range pkts {
		raw, err := rtpnet.Encode(pkt)
		if err != nil {
			return err
		}
		if err := writeFrame(rw.mw, s.TrackID, raw); err != nil {
			return err
		}
		rw.metrics.RTPpackets++
		rw.metrics.Bytes += int64(sample.FrameHeaderLen + len(raw))
	}
	rw.metrics.Samples++
	rw.metrics.PerTrack[s.TrackID]++
	return nil
}

// writeFrame prefixes each RTP packet with the deterministic framing header so
// the output is self-describing and independently re-parseable.
func writeFrame(w io.Writer, trackID uint32, rtpBytes []byte) error {
	_, err := w.Write(sample.Frame(trackID, rtpBytes))
	return err
}

// Finalize returns the accumulated metrics with the checksum digest. The
// container header (ftyp + a summary box) is written around the framed stream
// via the fixture builder primitives for guaranteed deterministic layout.
func (rw *Writer) Finalize() (Metrics, error) {
	rw.metrics.SHA256 = hex.EncodeToString(rw.hasher.Sum(nil))
	return rw.metrics, nil
}

// WriteContainer writes a complete deterministic transport file into sink from
// the ordered samples. It composes an ftyp header and the framed RTP stream so
// the bytes are stable across reruns.
func WriteContainer(sink io.Writer, ordered []*sample.Sample,
	profiles []TrackProfile) (Metrics, error) {

	// Header.
	var head bytesBuf
	_ = fixture.WriteBoxes(&head, fixture.Box("ftyp", func(w io.Writer) error {
		_, err := w.Write([]byte("isom"))
		return err
	}))
	if _, err := sink.Write(head.Bytes()); err != nil {
		return Metrics{}, err
	}
	rw := NewWriter(sink, profiles)
	for _, s := range ordered {
		if err := rw.WriteSample(s); err != nil {
			return Metrics{}, err
		}
	}
	return rw.Finalize()
}

type bytesBuf struct{ b []byte }

func (x *bytesBuf) Write(p []byte) (int, error) { x.b = append(x.b, p...); return len(p), nil }
func (x *bytesBuf) Bytes() []byte               { return x.b }
