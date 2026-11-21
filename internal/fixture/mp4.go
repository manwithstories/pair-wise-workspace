// Package fixture generates deterministic, self-contained demo inputs: HLS
// and DASH manifests, fragmented MP4 segments and an RTP packet capture.
package fixture

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
)

// Box writing helpers keep the fixture MP4 files small and explicit. Parsing
// them in the stream package goes through go-mp4.

func rawBox(typ string, payload ...[]byte) []byte {
	n := 8
	for _, p := range payload {
		n += len(p)
	}
	out := make([]byte, 8, n)
	binary.BigEndian.PutUint32(out, uint32(n))
	copy(out[4:8], typ)
	for _, p := range payload {
		out = append(out, p...)
	}
	return out
}

func fullBox(typ string, version byte, flags uint32, payload ...[]byte) []byte {
	hdr := make([]byte, 4)
	hdr[0] = version
	hdr[1] = byte(flags >> 16)
	hdr[2] = byte(flags >> 8)
	hdr[3] = byte(flags)
	return rawBox(typ, append([][]byte{hdr}, payload...)...)
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func u64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func u16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

var identityMatrix = []byte{
	0, 1, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 1, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0x40, 0, 0, 0,
}

func ftyp() []byte {
	return rawBox("ftyp", []byte("iso6"), u32(0), []byte("iso6isommp41"))
}

func mvhd(timescale, duration uint32) []byte {
	payload := [][]byte{
		u32(0), u32(0), u32(timescale), u32(duration),
		u32(0x00010000), u16(0x0100), u16(0),
		u32(0), u32(0), identityMatrix,
		make([]byte, 24), u32(3),
	}
	return fullBox("mvhd", 0, 0, payload...)
}

func tkhd(trackID, duration uint32, volume uint16, width, height uint32) []byte {
	payload := [][]byte{
		u32(0), u32(0), u32(trackID), u32(0), u32(duration),
		u32(0), u32(0), u16(0), u16(0), u16(volume), u16(0),
		identityMatrix, u32(width), u32(height),
	}
	return fullBox("tkhd", 0, 7, payload...)
}

func mdhd(timescale, duration uint32) []byte {
	// language "und" encoded as ISO-639-2/T packed 5-bit values.
	return fullBox("mdhd", 0, 0, u32(0), u32(0), u32(timescale), u32(duration), u16(0x55C4), u16(0))
}

func hdlr(handler string) []byte {
	name := append([]byte(handler), 0)
	return fullBox("hdlr", 0, 0, u32(0), []byte(handler), make([]byte, 12), name)
}

func trak(kind string, trackID, timescale, duration uint32, width, height uint32) []byte {
	volume := uint16(0)
	if kind == "soun" {
		volume = 0x0100
	}
	mdia := rawBox("mdia", mdhd(timescale, duration), hdlr(kind))
	return rawBox("trak", tkhd(trackID, duration, volume, width, height), mdia)
}

// initFile builds a two-track initialization segment: track 1 video and
// track 2 audio.
func initFile(videoTS, audioTS uint32, durationMS uint32) []byte {
	moov := rawBox("moov",
		mvhd(1000, durationMS),
		trak("vide", 1, videoTS, durationMS*videoTS/1000, 1920<<16, 1080<<16),
		trak("soun", 2, audioTS, durationMS*audioTS/1000, 0, 0),
	)
	return append(ftyp(), moov...)
}

// SegmentSpec describes one fragmented MP4 segment to emit.
type SegmentSpec struct {
	TrackID     uint32
	BaseTime    uint64 // track timescale ticks
	SampleDur   uint32
	SampleCount int
	SampleSize  uint32
	Keyframe    bool // when false every sample is a non-sync sample
	NoKeyframes bool // convenience: all samples non-sync
	CorruptBox  bool // write a trun with an impossible declared size
	Truncate    int  // remove this many trailing bytes from the finished file
}

// segmentFile assembles styp + moof + mdat. The trun carries per-sample
// duration, size and flags, so go-mp4 can recover exact sample timing.
func segmentFile(spec SegmentSpec, seq uint32) []byte {
	mfhd := fullBox("mfhd", 0, 0, u32(seq))
	tfhd := fullBox("tfhd", 0, 0x020000|0x000008, u32(spec.TrackID), u32(spec.SampleDur))
	tfdt := fullBox("tfdt", 1, 0, u64(spec.BaseTime))
	entries := make([]byte, 0, spec.SampleCount*12)
	for i := 0; i < spec.SampleCount; i++ {
		flags := uint32(0x02000000) // depends_on = I-frame
		if spec.NoKeyframes || (!spec.Keyframe && i > 0) {
			flags |= 0x00010000 // sample_is_non_sync_sample
		}
		entries = append(entries, u32(spec.SampleDur)...)
		entries = append(entries, u32(spec.SampleSize)...)
		entries = append(entries, u32(flags)...)
	}
	trunFlags := uint32(0x000001 | 0x000100 | 0x000200 | 0x000400)
	trun := fullBox("trun", 0, trunFlags, u32(uint32(spec.SampleCount)), u32(0), entries)
	if spec.CorruptBox {
		// Declared size 3 is smaller than the 8-byte box header: the walker
		// must reject it without reading out of bounds.
		trun[0], trun[1], trun[2], trun[3] = 0, 0, 0, 3
	}
	traf := rawBox("traf", tfhd, tfdt, trun)
	moof := rawBox("moof", mfhd, traf)
	payloadLen := spec.SampleCount * int(spec.SampleSize)
	if payloadLen < 64 {
		payloadLen = 64
	}
	mdatPayload := make([]byte, payloadLen)
	for i := range mdatPayload {
		mdatPayload[i] = byte((i*31 + int(seq)*7 + int(spec.TrackID)) % 251)
	}
	out := append(rawBox("styp", []byte("iso6"), u32(0), []byte("iso6")), moof...)
	out = append(out, rawBox("mdat", mdatPayload)...)
	if spec.Truncate > 0 && spec.Truncate < len(out) {
		out = out[:len(out)-spec.Truncate]
	}
	return out
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("fixture: write %s: %w", path, err)
	}
	return nil
}
