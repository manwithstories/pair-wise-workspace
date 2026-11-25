// Package demux deinterleaves Matroska/WebM block elements into timed tracks.
package demux

import (
	"bytes"
	"io"

	"mediaproc/internal/ebml"
)

// Track describes one Matroska track entry.
type Track struct {
	Number      uint64
	UID         uint64
	Type        int
	CodecID     string
	Channels    uint64
	TimeScale   uint64
	Unsupported bool
}

// Metadata contains segment-wide timing and track metadata.
type Metadata struct {
	TimecodeScaleNS uint64
	Tracks          map[uint64]*Track
}

// DefaultMetadata returns WebM's default one-millisecond block tick.
func DefaultMetadata() *Metadata {
	return &Metadata{TimecodeScaleNS: 1_000_000, Tracks: map[uint64]*Track{}}
}

func readUint(body []byte) uint64 {
	var v uint64
	for _, b := range body {
		v = v<<8 | uint64(b)
	}
	return v
}

func eachChild(payload []byte, fn func(el ebml.Element, body []byte) error) error {
	r, err := ebml.NewReader(bytes.NewReader(payload))
	if err != nil {
		return err
	}
	for r.Offset() < int64(len(payload)) {
		el, err := r.ReadElementHeader()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		body, err := r.ReadPayload(el, int64(len(payload)))
		if err != nil {
			return err
		}
		if err := fn(el, body); err != nil {
			return err
		}
	}
	return nil
}

// ParseInfo parses the subset of the Info element used for timing.
func ParseInfo(payload []byte) (timecodeScaleNS uint64) {
	timecodeScaleNS = 1_000_000
	_ = eachChild(payload, func(el ebml.Element, body []byte) error {
		if el.ID == ebml.IDTimecodeScale {
			if v := readUint(body); v != 0 {
				timecodeScaleNS = v
			}
		}
		return nil
	})
	return timecodeScaleNS
}

// ParseTracks parses TrackEntry elements and marks codecs without a decoder.
func ParseTracks(payload []byte) map[uint64]*Track {
	tracks := map[uint64]*Track{}
	_ = eachChild(payload, func(el ebml.Element, body []byte) error {
		if el.ID != ebml.IDTrackEntry {
			return nil
		}
		t := &Track{Channels: 1, TimeScale: 1_000_000}
		_ = eachChild(body, func(el ebml.Element, inner []byte) error {
			switch el.ID {
			case ebml.IDTrackNumber:
				t.Number = readUint(inner)
			case ebml.IDTrackUID:
				t.UID = readUint(inner)
			case ebml.IDTrackType:
				t.Type = int(readUint(inner))
			case ebml.IDCodecID:
				t.CodecID = string(inner)
			case ebml.IDChannels:
				t.Channels = readUint(inner)
			case ebml.IDAudio:
				_ = eachChild(inner, func(a ebml.Element, ab []byte) error {
					if a.ID == ebml.IDChannels {
						t.Channels = readUint(ab)
					}
					return nil
				})
			}
			return nil
		})
		switch t.CodecID {
		case "A_OPUS", "V_VP8", "V_VP9", "V_MPEG4/ISO/AVC":
			t.Unsupported = false
		default:
			t.Unsupported = t.CodecID != ""
		}
		if t.Number != 0 {
			tracks[t.Number] = t
		}
		return nil
	})
	return tracks
}
