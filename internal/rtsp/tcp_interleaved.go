package rtsp

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// ChunkKind distinguishes RTSP control bytes from interleaved RTP/RTCP.
type ChunkKind uint8

const (
	ChunkControl ChunkKind = iota + 1
	ChunkRTP
	ChunkRTCP
)

// Chunk is one complete RTSP response or interleaved channel frame.
type Chunk struct {
	Kind    ChunkKind
	Channel byte
	Data    []byte
}

// InterleavedParser incrementally splits an RTSP TCP byte stream. Incomplete
// headers, bodies, and $-frames stay cached until the next Feed call.
type InterleavedParser struct {
	buf []byte
	max int
}

// NewInterleavedParser creates a parser with a bounded assembly buffer.
func NewInterleavedParser() *InterleavedParser {
	return &InterleavedParser{max: 16 << 20}
}

// Feed appends bytes and returns every complete chunk that is now available.
func (p *InterleavedParser) Feed(data []byte) ([]Chunk, error) {
	p.buf = append(p.buf, data...)
	var chunks []Chunk
	for len(p.buf) > 0 {
		if p.buf[0] == '$' {
			if len(p.buf) < 4 {
				break
			}
			size := int(p.buf[2])<<8 | int(p.buf[3])
			if len(p.buf) < 4+size {
				break
			}
			chunk := Chunk{Channel: p.buf[1], Data: append([]byte(nil), p.buf[4:4+size]...)}
			switch chunk.Channel {
			case 0:
				chunk.Kind = ChunkRTP
			case 1:
				chunk.Kind = ChunkRTCP
			default:
				return nil, fmt.Errorf("unexpected interleaved channel %d", chunk.Channel)
			}
			chunks = append(chunks, chunk)
			p.buf = p.buf[4+size:]
			continue
		}
		if p.buf[0] != 'R' {
			return nil, fmt.Errorf("invalid RTSP response prefix 0x%02x", p.buf[0])
		}
		headerEnd := bytes.Index(p.buf, []byte("\r\n\r\n"))
		if headerEnd < 0 {
			break
		}
		headerEnd += 4
		bodyLen, err := responseBodyLength(p.buf[:headerEnd])
		if err != nil {
			return nil, err
		}
		total := headerEnd + bodyLen
		if total > p.max {
			return nil, fmt.Errorf("RTSP chunk exceeds %d bytes", p.max)
		}
		if len(p.buf) < total {
			break
		}
		chunks = append(chunks, Chunk{
			Kind: ChunkControl,
			Data: append([]byte(nil), p.buf[:total]...),
		})
		p.buf = p.buf[total:]
	}
	if len(p.buf) > p.max {
		return nil, fmt.Errorf("interleaved stream buffer exceeds %d bytes", p.max)
	}
	if len(p.buf) == 0 {
		p.buf = nil
	} else {
		p.buf = append([]byte(nil), p.buf...)
	}
	return chunks, nil
}

// Buffered reports the number of incomplete bytes retained.
func (p *InterleavedParser) Buffered() int { return len(p.buf) }

func responseBodyLength(header []byte) (int, error) {
	lines := strings.Split(string(header), "\r\n")
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "Content-Length") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid Content-Length %q", value)
		}
		return n, nil
	}
	return 0, nil
}
