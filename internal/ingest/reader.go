// Package ingest reads bounded length-prefixed RTP/RTCP datagrams and restores
// packet order inside a bounded sequence window.
package ingest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/pion/rtp"

	"github.com/clearliu/mediq/internal/core"
)

const (
	// DefaultMaxDatagramBytes is the demo's conservative datagram cap.
	DefaultMaxDatagramBytes = 8192
	// HardMaxDatagramBytes prevents a hostile length prefix from causing a
	// huge allocation even when callers request a larger configured cap.
	HardMaxDatagramBytes = 1 << 20
)

var (
	ErrBadLengthPrefix = errors.New("invalid length prefix")
	ErrDatagramTooLarge = errors.New("datagram exceeds configured limit")
	ErrBadRTPVersion    = errors.New("unsupported RTP version")
	ErrShortHeader      = errors.New("truncated RTP header")
	ErrBadHeaderLength  = errors.New("RTP header length exceeds datagram")
	ErrShortExtension   = errors.New("truncated RTP extension header")
	ErrBadExtensionLen  = errors.New("RTP extension length exceeds datagram")
	ErrShortSR          = errors.New("truncated RTCP sender report")
)

// Kind identifies a framed datagram's protocol family.
type Kind string

const (
	KindUnknown Kind = "unknown"
	KindRTP     Kind = "rtp"
	KindRTCP    Kind = "rtcp"
)

// SenderReport is the fixed sender-info portion of an RTCP SR.
type SenderReport struct {
	SSRC         uint32
	NTPSeconds   uint32
	NTPFraction  uint32
	RTPTimestamp uint32
	PacketCount  uint32
	OctetCount   uint32
}

// Record is one successfully parsed framed datagram.
type Record struct {
	Kind        Kind
	FrameOffset int64 // offset of the four-byte length prefix
	Offset      int64 // absolute payload offset in the input stream
	Length      int
	Raw         []byte
	Packet      *rtp.Packet
	SR          *SenderReport
}

// Reader incrementally reads four-byte big-endian length-prefixed datagrams.
type Reader struct {
	r       io.Reader
	offset  int64
	maxSize int
}

// NewReader wraps r. A non-positive maxSize selects the default cap.
func NewReader(r io.Reader, maxSize int) *Reader {
	if maxSize <= 0 {
		maxSize = DefaultMaxDatagramBytes
	}
	if maxSize > HardMaxDatagramBytes {
		maxSize = HardMaxDatagramBytes
	}
	return &Reader{r: r, maxSize: maxSize}
}

// Offset returns the number of input bytes consumed.
func (r *Reader) Offset() int64 { return r.offset }

// Next reads and parses the next frame. It returns io.EOF at a clean frame
// boundary. Parse errors are classified with the first relevant byte offset.
func (r *Reader) Next() (*Record, error) {
	prefixStart := r.offset
	var lengthBuf [4]byte
	n, err := io.ReadFull(r.r, lengthBuf[:])
	r.offset += int64(n)
	if errors.Is(err, io.EOF) && n == 0 {
		return nil, io.EOF
	}
	if err != nil {
		return nil, core.At(core.KindParse, "read-length-prefix", prefixStart+int64(n),
			fmt.Errorf("%w: %v", ErrBadLengthPrefix, err))
	}

	size := int(binary.BigEndian.Uint32(lengthBuf[:]))
	if size <= 0 {
		return nil, core.At(core.KindParse, "datagram-length", prefixStart,
			fmt.Errorf("%w: length=%d", ErrBadLengthPrefix, size))
	}
	if size > r.maxSize {
		// Do not allocate the advertised size. The frame cannot be skipped
		// safely without its body, so this is a classified input failure.
		return nil, core.At(core.KindParse, "datagram-length", prefixStart+4,
			fmt.Errorf("%w: length=%d limit=%d", ErrDatagramTooLarge, size, r.maxSize))
	}

	payloadStart := r.offset
	buf := make([]byte, size)
	n, err = io.ReadFull(r.r, buf)
	r.offset += int64(n)
	if err != nil {
		return nil, core.At(core.KindParse, "read-datagram", payloadStart+int64(n),
			fmt.Errorf("%w: %v", ErrShortHeader, err))
	}

	kind := Classify(buf)
	rec := &Record{Kind: kind, FrameOffset: prefixStart, Offset: payloadStart, Length: size, Raw: buf}
	switch kind {
	case KindRTP:
		if perr := ValidateRTP(buf, payloadStart); perr != nil {
			return rec, perr
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf); err != nil {
			return rec, core.At(core.KindParse, "unmarshal-rtp", payloadStart, err)
		}
		rec.Packet = &pkt
	case KindRTCP:
		sr, perr := ParseSenderReport(buf, payloadStart)
		if perr != nil {
			return rec, perr
		}
		rec.SR = sr
	default:
		return rec, core.At(core.KindParse, "classify-datagram", payloadStart,
			fmt.Errorf("first byte=0x%02x second byte=0x%02x", buf[0], buf[1]))
	}
	return rec, nil
}

// Classify performs minimal RTP/RTCP demultiplexing.
func Classify(b []byte) Kind {
	if len(b) < 2 {
		return KindUnknown
	}
	if b[0]>>6 != 2 {
		return KindUnknown
	}
	pt := b[1] & 0x7f
	// RTCP packet types occupy 200..207 in the second byte. RTP uses only the
	// low seven bits for payload type and our marker bit is 1<<7.
	if pt >= 72 && pt <= 79 && b[1] >= 200 && b[1] <= 207 {
		return KindRTCP
	}
	if b[1] >= 200 && b[1] <= 207 {
		return KindRTCP
	}
	_ = pt
	return KindRTP
}

// ValidateRTP checks fixed header, CSRC length and extension length before
// pion unmarshalling so truncation can carry a precise absolute offset.
func ValidateRTP(b []byte, base int64) error {
	if len(b) < 12 {
		return core.At(core.KindParse, "rtp-fixed-header", base+int64(len(b)),
			fmt.Errorf("%w: have=%d need=12", ErrShortHeader, len(b)))
	}
	if b[0]>>6 != 2 {
		return core.At(core.KindParse, "rtp-version", base, ErrBadRTPVersion)
	}

	csrcCount := int(b[0] & 0x0f)
	headerLen := 12 + 4*csrcCount
	if headerLen > len(b) {
		return core.At(core.KindParse, "rtp-csrc-header-length", base+12,
			fmt.Errorf("%w: cc=%d header_bytes=%d datagram=%d", ErrBadHeaderLength, csrcCount, headerLen, len(b)))
	}

	if b[0]&0x10 != 0 {
		extStart := headerLen
		if extStart+4 > len(b) {
			return core.At(core.KindParse, "rtp-extension-header", base+int64(extStart),
				fmt.Errorf("%w: extension starts at header byte %d", ErrShortExtension, extStart))
		}
		extWords := int(binary.BigEndian.Uint16(b[extStart+2 : extStart+4]))
		extEnd := extStart + 4 + 4*extWords
		if extEnd > len(b) {
			return core.At(core.KindParse, "rtp-extension-length", base+int64(extStart+2),
				fmt.Errorf("%w: declared_end=%d datagram=%d", ErrBadExtensionLen, extEnd, len(b)))
		}
	}
	return nil
}

// ParseSenderReport extracts the first SR's sender info.
func ParseSenderReport(b []byte, base int64) (*SenderReport, error) {
	if len(b) < 2 {
		return nil, core.At(core.KindParse, "rtcp-sr", base, ErrShortSR)
	}
	if b[1] != 200 {
		return nil, core.At(core.KindParse, "rtcp-type", base+1, fmt.Errorf("not SR: PT=%d", b[1]))
	}
	if len(b) < 28 {
		return nil, core.At(core.KindParse, "rtcp-sr-sender-info", base+int64(len(b)),
			fmt.Errorf("%w: have=%d need=28", ErrShortSR, len(b)))
	}
	return &SenderReport{
		SSRC:         binary.BigEndian.Uint32(b[4:8]),
		NTPSeconds:   binary.BigEndian.Uint32(b[8:12]),
		NTPFraction:  binary.BigEndian.Uint32(b[12:16]),
		RTPTimestamp: binary.BigEndian.Uint32(b[16:20]),
		PacketCount:  binary.BigEndian.Uint32(b[20:24]),
		OctetCount:   binary.BigEndian.Uint32(b[24:28]),
	}, nil
}
