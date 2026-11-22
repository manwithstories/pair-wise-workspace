package rtpdump

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math/rand"
	"os"
	"path/filepath"

	pionrtp "github.com/pion/rtp"
)

// Options controls a synthetic RTP dump.
type Options struct {
	Seed       int64
	Count      int // number of packets before loss
	SSRC       uint32
	PayloadMin int
	PayloadMax int
	ClockRate  uint32 // ticks per second used to advance timestamps
	Loss       float64
	Reorder    float64
	Duplicate  float64
}

// Report describes a generated dump and the ground truth applied.
type Report struct {
	Path           string
	SSRC           uint32
	Sent           int
	Dropped        int
	Duplicated     int
	Reordered      int
	Delivered      int
	ExpectedRate   float64
	FirstSeq       uint16
	ExpectedHashes []string
}

// Generate writes a synthetic RTP dump applying loss, reorder and
// duplication deterministically from the seed.
func Generate(dir, name string, opts Options) (*Report, error) {
	if opts.Count <= 0 {
		opts.Count = 600
	}
	if opts.SSRC == 0 {
		opts.SSRC = 0x51A9C0DE
	}
	if opts.PayloadMin <= 0 {
		opts.PayloadMin = 60
	}
	if opts.PayloadMax < opts.PayloadMin {
		opts.PayloadMax = opts.PayloadMin + 60
	}
	if opts.ClockRate == 0 {
		opts.ClockRate = 90000
	}
	if opts.Loss < 0 || opts.Loss >= 1 || opts.Reorder < 0 || opts.Reorder >= 1 ||
		opts.Duplicate < 0 || opts.Duplicate >= 1 {
		return nil, fmt.Errorf("fixture: probabilities must be in [0,1)")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)

	rng := rand.New(rand.NewSource(opts.Seed))
	var packets [][]byte
	report := &Report{Path: path, SSRC: opts.SSRC, FirstSeq: 1000}
	step := opts.ClockRate / uint32(opts.Count)
	if step == 0 {
		step = 1
	}
	var dropped, duped, reordered int
	for i := 0; i < opts.Count; i++ {
		if rng.Float64() < opts.Loss {
			dropped++
			continue
		}
		size := opts.PayloadMin + rng.Intn(opts.PayloadMax-opts.PayloadMin+1)
		payload := make([]byte, size)
		rng.Read(payload)
		payload[0] = 0xA5
		binary.BigEndian.PutUint32(payload[1:5], uint32(i))

		pkt := &pionrtp.Packet{
			Header: pionrtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: uint16(1000 + i),
				Timestamp:      uint32(i) * step,
				SSRC:           opts.SSRC,
			},
			Payload: payload,
		}
		raw, err := pkt.Marshal()
		if err != nil {
			return nil, err
		}
		packets = append(packets, raw)
		if rng.Float64() < opts.Duplicate {
			packets = append(packets, raw)
			duped++
		}
		// Reorder: swap with the next packet's position by deferring this one.
		if rng.Float64() < opts.Reorder && len(packets) > 1 {
			packets[len(packets)-1], packets[len(packets)-2] = packets[len(packets)-2], packets[len(packets)-1]
			reordered++
		}
	}

	if err := writeDump(path, opts.SSRC, packets); err != nil {
		return nil, err
	}

	report.Sent = opts.Count
	report.Dropped = dropped
	report.Duplicated = duped
	report.Reordered = reordered
	report.Delivered = opts.Count - dropped
	if total := report.Delivered + dropped; total > 0 {
		report.ExpectedRate = float64(report.Delivered) / float64(total)
	} else {
		report.ExpectedRate = 1
	}
	return report, nil
}

// writeDump encodes the RTPD framing (magic, version, ssrc, length+crc records)
// directly so this generator has no dependency on the demuxer under test.
func writeDump(path string, ssrc uint32, packets [][]byte) error {
	var buf bytes.Buffer
	buf.WriteString("RTPD")
	var hdr [8]byte
	binary.BigEndian.PutUint16(hdr[0:2], 1)
	binary.BigEndian.PutUint32(hdr[2:6], ssrc)
	buf.Write(hdr[:])
	for _, p := range packets {
		var meta [8]byte
		binary.BigEndian.PutUint32(meta[0:4], uint32(len(p)))
		binary.BigEndian.PutUint32(meta[4:8], crc32.ChecksumIEEE(p))
		buf.Write(meta[:])
		buf.Write(p)
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
