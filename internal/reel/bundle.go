// Package reel implements the deterministic, lossless frame bundle used as
// source material for the transcode pipeline.
package reel

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

const (
	BundleMagic   = "RCXREEL1"
	BundleVersion = 1
	headerSize    = 48
	frameHdrSize  = 16
)

var ErrCorrupt = errors.New("reel bundle is corrupt")

// Bundle is an in-memory lossless frame sequence. Frames are indexed as
// Segments[segment][frame] and every frame stores Width*Height luma bytes.
type Bundle struct {
	Seed         uint64
	Width        int
	Height       int
	FPS          int
	FramesPerSeg int
	Segments     [][][]byte
}

func (b *Bundle) Validate() error {
	if b.Width <= 0 || b.Height <= 0 || b.FPS <= 0 || b.FramesPerSeg <= 0 {
		return fmt.Errorf("invalid reel dimensions: %dx%d fps=%d frames=%d", b.Width, b.Height, b.FPS, b.FramesPerSeg)
	}
	if len(b.Segments) == 0 {
		return errors.New("reel has no segments")
	}
	want := b.Width * b.Height
	for si, seg := range b.Segments {
		if len(seg) != b.FramesPerSeg {
			return fmt.Errorf("segment %d: got %d frames, want %d", si+1, len(seg), b.FramesPerSeg)
		}
		for fi, frame := range seg {
			if len(frame) != want {
				return fmt.Errorf("segment %d frame %d: got %d bytes, want %d", si+1, fi, len(frame), want)
			}
		}
	}
	return nil
}

func (b *Bundle) TotalFrames() int { return len(b.Segments) * b.FramesPerSeg }

// Encode serializes the bundle. The format is little-endian and contains no
// timestamps or map iteration, so equal inputs produce identical bytes.
func (b *Bundle) Encode() ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	payloadLen := 0
	for _, seg := range b.Segments {
		for _, frame := range seg {
			payloadLen += frameHdrSize + len(frame)
		}
	}
	if payloadLen > int(^uint32(0)) {
		return nil, errors.New("reel payload too large")
	}
	payload := make([]byte, 0, payloadLen)
	var frameHdr [frameHdrSize]byte
	for si, seg := range b.Segments {
		for fi, frame := range seg {
			binary.LittleEndian.PutUint16(frameHdr[0:2], uint16(si+1))
			binary.LittleEndian.PutUint16(frameHdr[2:4], uint16(fi))
			binary.LittleEndian.PutUint32(frameHdr[4:8], uint32(len(frame)))
			binary.LittleEndian.PutUint32(frameHdr[8:12], crc32.ChecksumIEEE(frame))
			binary.LittleEndian.PutUint32(frameHdr[12:16], 0)
			payload = append(payload, frameHdr[:]...)
			payload = append(payload, frame...)
		}
	}
	out := make([]byte, headerSize+len(payload))
	copy(out[0:8], BundleMagic)
	binary.LittleEndian.PutUint16(out[8:10], BundleVersion)
	binary.LittleEndian.PutUint16(out[10:12], uint16(b.Width))
	binary.LittleEndian.PutUint16(out[12:14], uint16(b.Height))
	binary.LittleEndian.PutUint16(out[14:16], uint16(b.FPS))
	binary.LittleEndian.PutUint16(out[16:18], uint16(len(b.Segments)))
	binary.LittleEndian.PutUint16(out[18:20], uint16(b.FramesPerSeg))
	binary.LittleEndian.PutUint64(out[24:32], b.Seed)
	binary.LittleEndian.PutUint64(out[32:40], uint64(len(payload)))
	binary.LittleEndian.PutUint32(out[40:44], crc32.ChecksumIEEE(payload))
	binary.LittleEndian.PutUint32(out[44:48], crc32.ChecksumIEEE(out[:44]))
	copy(out[headerSize:], payload)
	return out, nil
}

func Decode(data []byte) (*Bundle, error) {
	if len(data) < headerSize {
		return nil, fmt.Errorf("%w: truncated header", ErrCorrupt)
	}
	if string(data[0:8]) != BundleMagic {
		return nil, fmt.Errorf("%w: bad magic", ErrCorrupt)
	}
	if binary.LittleEndian.Uint16(data[8:10]) != BundleVersion {
		return nil, fmt.Errorf("%w: unsupported version", ErrCorrupt)
	}
	if crc32.ChecksumIEEE(data[:44]) != binary.LittleEndian.Uint32(data[44:48]) {
		return nil, fmt.Errorf("%w: header checksum mismatch", ErrCorrupt)
	}
	b := &Bundle{
		Seed:         binary.LittleEndian.Uint64(data[24:32]),
		Width:        int(binary.LittleEndian.Uint16(data[10:12])),
		Height:       int(binary.LittleEndian.Uint16(data[12:14])),
		FPS:          int(binary.LittleEndian.Uint16(data[14:16])),
		FramesPerSeg: int(binary.LittleEndian.Uint16(data[18:20])),
	}
	nseg := int(binary.LittleEndian.Uint16(data[16:18]))
	payloadLen := binary.LittleEndian.Uint64(data[32:40])
	if payloadLen != uint64(len(data)-headerSize) {
		return nil, fmt.Errorf("%w: payload length mismatch", ErrCorrupt)
	}
	payload := data[headerSize:]
	if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(data[40:44]) {
		return nil, fmt.Errorf("%w: payload checksum mismatch", ErrCorrupt)
	}
	if b.Width <= 0 || b.Height <= 0 || b.FPS <= 0 || nseg <= 0 || b.FramesPerSeg <= 0 {
		return nil, fmt.Errorf("%w: invalid dimensions", ErrCorrupt)
	}
	if b.Width*b.Height > 64<<20 {
		return nil, fmt.Errorf("%w: frame too large", ErrCorrupt)
	}
	b.Segments = make([][][]byte, nseg)
	for si := range b.Segments {
		b.Segments[si] = make([][]byte, b.FramesPerSeg)
	}
	off := 0
	for si := 0; si < nseg; si++ {
		for fi := 0; fi < b.FramesPerSeg; fi++ {
			if off+frameHdrSize > len(payload) {
				return nil, fmt.Errorf("%w: truncated frame header", ErrCorrupt)
			}
			h := payload[off : off+frameHdrSize]
			segID := int(binary.LittleEndian.Uint16(h[0:2]))
			frameID := int(binary.LittleEndian.Uint16(h[2:4]))
			n := int(binary.LittleEndian.Uint32(h[4:8]))
			wantCRC := binary.LittleEndian.Uint32(h[8:12])
			off += frameHdrSize
			if segID != si+1 || frameID != fi || n != b.Width*b.Height || off+n > len(payload) {
				return nil, fmt.Errorf("%w: invalid frame metadata", ErrCorrupt)
			}
			frame := payload[off : off+n]
			if crc32.ChecksumIEEE(frame) != wantCRC {
				return nil, fmt.Errorf("%w: frame checksum mismatch", ErrCorrupt)
			}
			b.Segments[si][fi] = append([]byte(nil), frame...)
			off += n
		}
	}
	if off != len(payload) {
		return nil, fmt.Errorf("%w: trailing payload", ErrCorrupt)
	}
	return b, nil
}

func Write(path string, b *Bundle) error {
	data, err := b.Encode()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

func Read(path string) (*Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decode(data)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".rcx-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return syncDir(dir)
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func CopyEncode(w io.Writer, b *Bundle) error {
	data, err := b.Encode()
	if err != nil {
		return err
	}
	_, err = io.Copy(w, bytes.NewReader(data))
	return err
}
