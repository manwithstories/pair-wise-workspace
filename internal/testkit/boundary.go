package testkit

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pion/rtp"
)

// RTPStreamSpec describes a synthetic RTP packet stream. Packets are written
// as length-prefixed raw RTP so the pion parser drives the timeline exactly as
// it would for a live capture.
type RTPStreamSpec struct {
	Name      string
	ClockRate uint32
	Count     int
	// TimestampStep is the RTP timestamp increment between packets.
	TimestampStep uint32
	// StartTimestamp is the first timestamp; choose a value near the 32-bit
	// wrap point to exercise unwrapping.
	StartTimestamp uint32
	// SequenceGapAfter inserts a sequence-number gap after this many packets
	// (0 disables), modelling packet loss.
	SequenceGapAfter int
	// StartSeq is the first sequence number.
	StartSeq uint16
}

// RTPStream is a generated stream and its expected presentation times.
type RTPStream struct {
	Spec    RTPStreamSpec
	Path    string
	Packets []rtp.Packet
	TimesUS []int64
	Wraps   int
}

// GenerateRTPStream writes a length-prefixed RTP stream into dir.
func GenerateRTPStream(dir string, spec RTPStreamSpec) (*RTPStream, error) {
	if spec.ClockRate == 0 || spec.Count <= 0 {
		return nil, fmt.Errorf("rtp spec needs a clock rate and at least one packet")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	out := &RTPStream{Spec: spec}
	buf := make([]byte, 0, spec.Count*32)
	seq := spec.StartSeq
	ts := spec.StartTimestamp
	prev := ts
	for i := 0; i < spec.Count; i++ {
		if i > 0 {
			ts += spec.TimestampStep
			if ts < prev {
				out.Wraps++
			}
		}
		if spec.SequenceGapAfter > 0 && i == spec.SequenceGapAfter {
			seq += 2
		}
		pkt := rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: seq,
				Timestamp:      ts,
				SSRC:           0x4d4c4e31,
			},
			Payload: []byte{0x4d, 0x4c, 0x4e, 0x31, byte(i), 0, 0, 0},
		}
		raw, err := pkt.Marshal()
		if err != nil {
			return nil, err
		}
		var lp [4]byte
		binary.BigEndian.PutUint32(lp[:], uint32(len(raw)))
		buf = append(buf, lp[:]...)
		buf = append(buf, raw...)
		out.Packets = append(out.Packets, pkt)
		out.TimesUS = append(out.TimesUS, int64(i)*int64(spec.TimestampStep)*1_000_000/int64(spec.ClockRate))
		prev = ts
		seq++
	}
	path := filepath.Join(dir, spec.Name+".rtp")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		return nil, err
	}
	out.Path = path
	return out, nil
}

// ReadRTPStream parses a generated length-prefixed stream.
func ReadRTPStream(path string) ([]rtp.Packet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []rtp.Packet
	for len(data) >= 4 {
		n := int(binary.BigEndian.Uint32(data[:4]))
		data = data[4:]
		if n < 0 || n > len(data) {
			return nil, fmt.Errorf("rtp frame length %d exceeds remaining %d", n, len(data))
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(data[:n]); err != nil {
			return nil, err
		}
		out = append(out, pkt)
		data = data[n:]
	}
	if len(data) != 0 {
		return nil, fmt.Errorf("trailing bytes in rtp stream")
	}
	return out, nil
}

// ZeroDurationClip writes a clip whose stts declares a second sample duration
// of zero, exercising the zero-duration guard. The entry count stays the same,
// so only the timing table is anomalous.
func ZeroDurationClip(dir string) (*Clip, error) {
	spec := ClipSpec{Name: "zerodur", Seed: 9, Tracks: []TrackSpec{DefaultVideoTrack(1, 2_000_000, 15)}}
	clip, err := GenerateClip(dir, spec)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(clip.Path, os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	off, err := findBoxInFile(f, 0, clip.Size, "stts")
	if err != nil {
		return nil, err
	}
	// stts layout: size(4) type(4) version/flags(4) entryCount(4)
	//              entries: sampleCount(4) sampleDelta(4)
	// The generator emits a single run; add a second entry with delta 0 by
	// rewriting entryCount and the following bytes is not possible without
	// changing box size, so instead set the single delta to 0. That models a
	// track whose only timing entry is zero-duration.
	deltaOff := off + 8 + 4 + 4 + 4
	var zero [4]byte
	if _, err := f.WriteAt(zero[:], int64(deltaOff)); err != nil {
		return nil, err
	}
	return clip, nil
}

// findBoxInFile recursively locates a box by type within [start,start+size).
func findBoxInFile(f *os.File, start, size int64, name string) (int64, error) {
	buf := make([]byte, 8)
	for off := start; off+8 <= start+size; {
		if _, err := f.ReadAt(buf, off); err != nil {
			return 0, err
		}
		bsize := int64(binary.BigEndian.Uint32(buf[:4]))
		if bsize < 8 || off+bsize > start+size {
			return 0, fmt.Errorf("bad box size %d at %d", bsize, off)
		}
		if string(buf[4:8]) == name {
			return off, nil
		}
		switch string(buf[4:8]) {
		case "moov", "trak", "mdia", "minf", "stbl", "edts", "dinf":
			if got, err := findBoxNested(f, off+8, bsize-8, name); err == nil {
				return got, nil
			}
		}
		off += bsize
	}
	return 0, fmt.Errorf("box %s not found", name)
}

func findBoxNested(f *os.File, start, size int64, name string) (int64, error) {
	buf := make([]byte, 8)
	for off := start; off+8 <= start+size; {
		if _, err := f.ReadAt(buf, off); err != nil {
			return 0, err
		}
		bsize := int64(binary.BigEndian.Uint32(buf[:4]))
		if bsize < 8 || off+bsize > start+size {
			return 0, fmt.Errorf("bad box size")
		}
		if string(buf[4:8]) == name {
			return off, nil
		}
		switch string(buf[4:8]) {
		case "moov", "trak", "mdia", "minf", "stbl", "edts", "dinf":
			if got, err := findBoxNested(f, off+8, bsize-8, name); err == nil {
				return got, nil
			}
		}
		off += bsize
	}
	return 0, fmt.Errorf("box %s not found", name)
}

// HugeLengthClip writes a clip whose mdat declares a length far beyond the
// file, exercising the invalid-length guard.
func HugeLengthClip(dir string) (*Clip, error) {
	clip, err := GenerateClip(dir, ClipSpec{Name: "huge", Seed: 5, Tracks: []TrackSpec{DefaultVideoTrack(1, 2_000_000, 15)}})
	if err != nil {
		return nil, err
	}
	if err := CorruptLength(clip.Path, clip.MdatOffset, 0xfffffff0); err != nil {
		return nil, err
	}
	return clip, nil
}
