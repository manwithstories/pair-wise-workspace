package rtp

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	pionrtp "github.com/pion/rtp"
)

// ReadCapture decodes a capture file. Two formats are accepted and detected by
// magic: the archivist binary format produced by the fixture generator, and a
// JSON-Lines dump with one base64 payload per line (see ReadJSONL).
func ReadCapture(path string) ([]Packet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Decode(f)
}

// Decode reads from r without closing it.
func Decode(r io.Reader) ([]Packet, error) {
	br := bufio.NewReaderSize(r, 1<<16)
	magic, err := br.Peek(len(captureMagic))
	if err != nil {
		return nil, fmt.Errorf("capture too short: %w", err)
	}
	if string(magic) == captureMagic {
		if _, err := br.Discard(len(captureMagic)); err != nil {
			return nil, err
		}
		return decodeBinary(br)
	}
	return decodeJSONL(br)
}

const captureMagic = "ARCRTP01"

// DecodeRecords turns raw pion RTP packets into capture records.
func DecodeRecords(raw [][]byte) ([]Packet, error) {
	out := make([]Packet, 0, len(raw))
	for i, b := range raw {
		p, err := decodePacket(b)
		if err != nil {
			return nil, fmt.Errorf("packet %d: %w", i, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// decodePacket applies pion's parser and lifts the extended clock out of the
// header extension when the sender provided one.
func decodePacket(b []byte) (Packet, error) {
	var p pionrtp.Packet
	if err := p.Unmarshal(b); err != nil {
		return Packet{}, err
	}
	pk := Packet{
		Seq:       p.SequenceNumber,
		Timestamp: p.Timestamp,
		SSRC:      p.SSRC,
		Marker:    p.Marker,
		Payload:   p.Payload,
	}
	if ext := p.GetExtension(ExtIDTimestamp); len(ext) == extensionBytesLen {
		pk.ExtTimestamp = int64(binary.BigEndian.Uint64(ext))
		pk.HasExt = true
	}
	return pk, nil
}

// maxCapturePackets bounds a run-length field read from an untrusted header.
const maxCapturePackets = 1 << 22

func decodeBinary(r io.Reader) ([]Packet, error) {
	var count uint32
	if err := binary.Read(r, binary.BigEndian, &count); err != nil {
		return nil, fmt.Errorf("capture header: %w", err)
	}
	if count > maxCapturePackets {
		return nil, fmt.Errorf("capture declares %d packets, refusing", count)
	}
	out := make([]Packet, 0, count)
	var arrival int64
	var seq uint16
	var length uint32
	for i := uint32(0); i < count; i++ {
		if err := binary.Read(r, binary.BigEndian, &arrival); err != nil {
			return nil, fmt.Errorf("record %d arrival: %w", i, err)
		}
		if err := binary.Read(r, binary.BigEndian, &seq); err != nil {
			return nil, fmt.Errorf("record %d seq: %w", i, err)
		}
		if err := binary.Read(r, binary.BigEndian, &length); err != nil {
			return nil, fmt.Errorf("record %d length: %w", i, err)
		}
		if length > 64<<10 {
			return nil, fmt.Errorf("record %d declares %d payload bytes, refusing", i, length)
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, fmt.Errorf("record %d payload: %w", i, err)
		}
		p, err := decodePacket(buf)
		if err != nil {
			return nil, fmt.Errorf("record %d: %w", i, err)
		}
		if p.Seq != seq {
			return nil, fmt.Errorf("record %d sequence mismatch: header %d packet %d", i, seq, p.Seq)
		}
		p.ArrivalUS = arrival
		out = append(out, p)
	}
	return out, nil
}

func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\t' || b[start] == '\r' || b[start] == '\n') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r' || b[end-1] == '\n') {
		end--
	}
	return b[start:end]
}
