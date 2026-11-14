package codec

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

type DecodedSegment struct {
	Meta        SegmentMeta
	Frames      [][]byte
	PSNR        float64
	CRC         uint32
	Bits        uint64
	FrameHashes [][32]byte
}

// DecodeSegment validates all checksums and decoded-frame hashes before
// returning reconstructed frames.
func DecodeSegment(data []byte) (*DecodedSegment, error) {
	if len(data) < SegmentHeaderSize+4 {
		return nil, fmt.Errorf("%w: truncated segment", ErrCorrupt)
	}
	header, err := parseSegmentHeader(data[:SegmentHeaderSize])
	if err != nil {
		return nil, err
	}
	payloadEnd := SegmentHeaderSize + int(header.PayloadLen)
	if payloadEnd+4 != len(data) {
		return nil, fmt.Errorf("%w: segment payload length mismatch", ErrCorrupt)
	}
	payload := data[SegmentHeaderSize:payloadEnd]
	if crc32.ChecksumIEEE(payload) != header.PayloadCRC {
		return nil, fmt.Errorf("%w: segment payload checksum mismatch", ErrCorrupt)
	}
	fileCRC := binary.LittleEndian.Uint32(data[payloadEnd:])
	if crc32.ChecksumIEEE(data[:payloadEnd]) != fileCRC {
		return nil, fmt.Errorf("%w: segment file checksum mismatch", ErrCorrupt)
	}
	meta := SegmentMeta{
		ID:     int(header.ID),
		QP:     int(header.QP),
		Width:  int(header.Width),
		Height: int(header.Height),
		FPS:    int(header.FPS),
		Frames: int(header.Frames),
	}
	decoded := &DecodedSegment{
		Meta:        meta,
		Frames:      make([][]byte, 0, header.Frames),
		PSNR:        header.PSNR,
		CRC:         fileCRC,
		Bits:        uint64(len(data)) * 8,
		FrameHashes: make([][32]byte, 0, header.Frames),
	}
	off := 0
	for i := 0; i < int(header.Frames); i++ {
		if off+FrameMetaSize > len(payload) {
			return nil, fmt.Errorf("%w: truncated frame metadata", ErrCorrupt)
		}
		fm := getFrameMeta(payload[off : off+FrameMetaSize])
		off += FrameMetaSize
		if int(fm.index) != i || int(fm.payloadLen) > len(payload)-off {
			return nil, fmt.Errorf("%w: invalid frame metadata", ErrCorrupt)
		}
		framePayload := payload[off : off+int(fm.payloadLen)]
		off += int(fm.payloadLen)
		if crc32.ChecksumIEEE(framePayload) != fm.payloadCRC {
			return nil, fmt.Errorf("%w: frame checksum mismatch", ErrCorrupt)
		}
		frame, err := DecodeFrame(framePayload, meta.Width, meta.Height, meta.QP)
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256(frame)
		if !bytes.Equal(hash[:], fm.decodedHash[:]) {
			return nil, fmt.Errorf("%w: decoded frame hash mismatch", ErrCorrupt)
		}
		decoded.Frames = append(decoded.Frames, frame)
		decoded.FrameHashes = append(decoded.FrameHashes, hash)
	}
	if off != len(payload) {
		return nil, fmt.Errorf("%w: trailing frame payload", ErrCorrupt)
	}
	return decoded, nil
}
