package ingest

import (
	"fmt"
	"io"
	"os"

	"mediarun/internal/mediaerr"
	"mediarun/internal/timebase"
)

// MXF OP1a subset supported here is exactly the subset the fixture generator
// (testdata/generate.go) writes. It is a deliberately small, self-describing
// framing so no external MXF tooling is required:
//
//	Partition Pack : UL(16) + BER length + payload
//	                 payload: BER item count, then `count` tagged items:
//	                   tag(2) + BER len + bytes  (tags: 1=major version,
//	                   2=kind "video"/"audio", 3=codec, 4=timescale BER,
//	                   5=width BER, 6=height BER, 7=channels BER,
//	                   8=sample count BER)
//	Essence samples: one KLV per sample, UL identifies the track; the length
//	                 is BER-encoded and bounds the payload bytes.
//
// Every length is validated against the file size: a truncated KLV becomes
// TRUNCATED_KLV with the exact offset instead of a tolerated short read.

// mxfUL is the 16-byte SMPTE-style universal label prefix.
var (
	mxfPartitionUL = [16]byte{0x06, 0x0e, 0x2b, 0x34, 0x01, 0x01, 0x01, 0xff,
		0x0d, 0x01, 0x02, 0x01, 0x10, 0x01, 0x00, 0x00}
	mxfVideoEssenceUL = [16]byte{0x06, 0x0e, 0x2b, 0x34, 0x01, 0x02, 0x01, 0x01,
		0x0d, 0x01, 0x03, 0x01, 0x05, 0x01, 0x02, 0x01}
	mxfAudioEssenceUL = [16]byte{0x06, 0x0e, 0x2b, 0x34, 0x01, 0x02, 0x01, 0x01,
		0x0d, 0x01, 0x03, 0x01, 0x05, 0x01, 0x02, 0x02}
)

type mxfStreamer struct {
	path   string
	file   *os.File
	size   int64
	pos    int64
	info   ContainerInfo
	tracks map[string]*mxfTrack
	order  []string // track IDs in partition declaration order
	picked int64    // total samples emitted
	totalN int64    // total samples advertised across tracks
}

type mxfTrack struct {
	desc           TrackDesc
	tb             timebase.Timebase
	next           int64
	hasDTS         bool
	ticksPerSample int64 // duration of one edit unit in this track's clock
}

// OpenMXF opens and parses the partition pack of an OP1a-subset file.
func OpenMXF(path string) (Streamer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(path)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(path)
	}
	s := &mxfStreamer{
		path: path, file: f, size: st.Size(),
		info:   ContainerInfo{Format: "mxf", Path: path, Size: st.Size()},
		tracks: map[string]*mxfTrack{},
	}
	if err := s.parsePartition(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return s, nil
}

func (s *mxfStreamer) Info() ContainerInfo { return s.info }
func (s *mxfStreamer) Close() error        { return s.file.Close() }

// readBER decodes an SMPTE BER length at the current position. First byte
// 0..0x7f is a short form length; the high-bit count gives long-form bytes.
func (s *mxfStreamer) readBER(what string) (int64, error) {
	first := make([]byte, 1)
	if _, err := io.ReadFull(s.file, first); err != nil {
		return 0, mediaerr.New(mediaerr.CodeTruncatedKLV,
			"truncated BER length for "+what).At("mxf", "", s.pos).OnPath(s.path)
	}
	s.pos++
	b0 := first[0]
	if b0 < 0x80 {
		return int64(b0), nil
	}
	n := int(b0 & 0x7f)
	if n == 0 || n > 8 {
		return 0, mediaerr.New(mediaerr.CodeUnknownStructure, fmt.Sprintf(
			"invalid BER long-form byte %#x for %s", b0, what)).
			At("mxf", "", s.pos-1).OnPath(s.path)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(s.file, buf); err != nil {
		return 0, mediaerr.New(mediaerr.CodeTruncatedKLV,
			"truncated BER long-form for "+what).At("mxf", "", s.pos).OnPath(s.path)
	}
	s.pos += int64(n)
	var v int64
	for _, c := range buf {
		v = (v << 8) | int64(c)
	}
	return v, nil
}

func (s *mxfStreamer) readUL(what string) ([16]byte, error) {
	var ul [16]byte
	if _, err := io.ReadFull(s.file, ul[:]); err != nil {
		return ul, mediaerr.New(mediaerr.CodeTruncatedKLV,
			"truncated UL for "+what).At("mxf", "", s.pos).OnPath(s.path)
	}
	s.pos += 16
	return ul, nil
}
