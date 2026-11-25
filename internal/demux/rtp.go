package demux

import (
	"errors"
	"io"

	"github.com/pion/rtp"

	"archivecheck/internal/media"
)

// H264 NAL unit type constants (RFC 6184).
const (
	nalTypeSTAPA  = 24 // single-time aggregation packet A
	nalTypeFUA    = 28 // fragmentation unit A
	nalTypeIDR    = 5
	nalTypeSPS    = 7
	nalTypePPS    = 8
)

// RTPConfig bounds the reassembler. MaxAUBytes caps one reconstructed access
// unit so a lost FU terminator cannot make the buffer grow without limit.
type RTPConfig struct {
	MaxAUBytes int
}

// DefaultRTPConfig mirrors the archive per-sample memory limits.
func DefaultRTPConfig() RTPConfig {
	return RTPConfig{MaxAUBytes: 1 << 20}
}

// H264Reassembler turns RFC 6184 RTP packets into complete H.264 access units
// (our Samples). It handles STAP-A aggregation and FU-A fragmentation, unwraps
// the 32-bit timestamp cycle and reports located faults instead of panicking
// on capture truncation.
type H264Reassembler struct {
	cfg      RTPConfig
	segment  string
	faults   media.FaultSet
	clock    media.Rational

	// FU-A accumulation state.
	fuActive   bool
	fuNalHeader byte   // reconstructed first byte of the fragmented NAL
	fuBuf      []byte
	fuTS       uint32 // RTP timestamp owning the in-progress FU
	fuStartOff int64

	// Access-unit completion is driven by timestamp changes and marker bits.
	lastTS    uint32
	haveLast  bool
	unwrapped int64 // monotonic 64-bit timestamp
	auData    []byte
	auTS      uint32
	auKey     bool
	auOff     int64
	auIndex   int
	pktIndex  int
	scanBytes int64
	samples   []Sample
}

// NewH264Reassembler builds a reassembler for one capture stream.
func NewH264Reassembler(segment string, cfg RTPConfig) *H264Reassembler {
	return &H264Reassembler{
		cfg:     cfg,
		segment: segment,
		clock:   media.TimeBaseVideo90k,
	}
}

// ProcessPacket feeds one raw RTP datagram. off is its byte offset inside the
// capture file and is attached to every fault and emitted sample.
func (r *H264Reassembler) ProcessPacket(raw []byte, off int64) error {
	r.pktIndex++
	r.scanBytes += int64(len(raw))
	var pkt rtp.Packet
	if err := pkt.Unmarshal(raw); err != nil {
		r.faults.Add(media.NewFault(media.KindBadLength, r.segment, 1, off,
			"RTP packet %d unmarshal failed: %s", r.pktIndex, err))
		return err
	}

	// Timestamp ordering: multiple packets of one access unit (FU-A fragments,
	// STAP-A) legally share a timestamp. Only a real backwards jump is a wrap.
	if r.haveLast && pkt.Timestamp != r.lastTS {
		if _, _, err := media.UnwrapRTP(r.lastTS, pkt.Timestamp); err != nil {
			r.faults.Add(media.NewFault(media.KindWrap, r.segment, 1, off,
				"RTP timestamp went backwards %d -> %d", r.lastTS, pkt.Timestamp))
		}
		if delta := int64(int32(pkt.Timestamp - r.lastTS)); delta > 0 {
			r.unwrapped += delta
		}
	} else if !r.haveLast {
		r.haveLast = true
		r.auTS = pkt.Timestamp
		r.auOff = off
	}

	// Timestamp change flushes the previous access unit before new NALs arrive.
	if pkt.Timestamp != r.auTS && r.auData != nil {
		r.flush()
		r.auTS = pkt.Timestamp
		r.auOff = off
	}

	if err := r.processNAL(pkt.Payload, off); err != nil {
		return err
	}
	r.lastTS = pkt.Timestamp

	// Marker bit marks the last packet of an access unit.
	if pkt.Marker && r.auData != nil {
		r.flush()
	}
	return nil
}

func (r *H264Reassembler) processNAL(payload []byte, off int64) error {
	if len(payload) == 0 {
		r.faults.Add(media.NewFault(media.KindBadLength, r.segment, 1, off, "empty RTP payload"))
		return errors.New("empty payload")
	}
	nalType := payload[0] & 0x1F
	switch {
	case nalType >= 1 && nalType <= 23:
		// Single NAL unit packet.
		r.appendNAL(append([]byte(nil), payload...), off)
	case nalType == nalTypeSTAPA:
		return r.processSTAPA(payload[1:], off)
	case nalType == nalTypeFUA:
		return r.processFUA(payload, off)
	default:
		// Reserved/undefined NAL types are skipped, not fatal to the stream.
		r.faults.Add(media.NewFault(media.KindUnsupported, r.segment, 1, off,
			"unsupported NAL type %d, skipping packet", nalType))
	}
	return nil
}

func (r *H264Reassembler) processSTAPA(body []byte, off int64) error {
	pos := 0
	for pos+2 <= len(body) {
		size := int(body[pos])<<8 | int(body[pos+1])
		pos += 2
		if size == 0 || pos+size > len(body) {
			r.faults.Add(media.NewFault(media.KindBadLength, r.segment, 1, off,
				"STAP-A declared size %d exceeds %d remaining bytes", size, len(body)-pos))
			return errors.New("corrupt STAP-A")
		}
		nal := append([]byte(nil), body[pos:pos+size]...)
		r.appendNAL(nal, off)
		pos += size
	}
	return nil
}

func (r *H264Reassembler) processFUA(payload []byte, off int64) error {
	if len(payload) < 2 {
		r.faults.Add(media.NewFault(media.KindBadLength, r.segment, 1, off, "FU-A shorter than 2 bytes"))
		return errors.New("short FU-A")
	}
	indicator, fuHeader := payload[0], payload[1]
	start := fuHeader&0x80 != 0
	end := fuHeader&0x40 != 0
	if start {
		r.fuActive = true
		r.fuNalHeader = (indicator & 0xE0) | (fuHeader & 0x1F)
		r.fuBuf = append(r.fuBuf[:0], r.fuNalHeader)
		r.fuTS = r.auTS
		r.fuStartOff = off
	}
	if !r.fuActive {
		// Capture started mid-FU: skip until the next start, no fake keyframe.
		return nil
	}
	if len(r.fuBuf)+len(payload)-2 > r.cfg.MaxAUBytes {
		r.fuActive = false
		r.fuBuf = nil
		r.faults.Add(media.NewFault(media.KindBadLength, r.segment, 1, off,
			"FU-A reassembly exceeded %d byte cap", r.cfg.MaxAUBytes))
		return errors.New("FU-A cap")
	}
	r.fuBuf = append(r.fuBuf, payload[2:]...)
	if end {
		nal := append([]byte(nil), r.fuBuf...)
		r.fuActive = false
		r.fuBuf = nil
		r.appendNAL(nal, r.fuStartOff)
	}
	return nil
}

// appendNAL appends one reconstructed NAL (with length-prefix style framing for
// the remuxer) to the current access unit.
func (r *H264Reassembler) appendNAL(nal []byte, off int64) {
	t := nal[0] & 0x1F
	if t == nalTypeIDR || t == nalTypeSPS || t == nalTypePPS {
		r.auKey = true
	}
	if r.auData == nil {
		r.auOff = off
	}
	// 4-byte AVCC length prefix keeps the sample self-delimited for BMFF mdat.
	l := make([]byte, 4)
	l[0], l[1], l[2], l[3] = byte(len(nal)>>24), byte(len(nal)>>16), byte(len(nal)>>8), byte(len(nal))
	r.auData = append(r.auData, l...)
	r.auData = append(r.auData, nal...)
}

func (r *H264Reassembler) flush() {
	if r.auData == nil {
		return
	}
	r.samples = append(r.samples, Sample{
		Track: 1, Index: r.auIndex, Data: append([]byte(nil), r.auData...),
		Offset: r.auOff, Duration: 0, DTS: r.unwrapped, Keyframe: r.auKey,
		Codec: CodecAVC1, TimeBase: r.clock,
	})
	r.auIndex++
	r.auData = nil
	r.auKey = false
}

// Finish marks end of stream. An unterminated FU means the capture was cut; it
// is reported as truncation and discarded rather than emitted half-formed.
func (r *H264Reassembler) Finish() ([]Sample, *SourceReport) {
	if r.fuActive {
		r.faults.Add(media.NewFault(media.KindTruncated, r.segment, 1, r.fuStartOff,
			"capture ended inside FU-A, dropping incomplete access unit"))
		r.fuActive = false
	}
	r.flush()
	report := &SourceReport{
		Segment: r.segment, Container: ContainerRTP, Samples: len(r.samples),
		Faults: r.faults.All(), ScanBytes: r.scanBytes,
	}
	out := r.samples
	r.samples = nil
	return out, report
}

// ReadRTPStream consumes length-prefixed RTP datagrams from r. The capture
// framing is a 2-byte big-endian length per datagram, as produced by the
// fixture generator; EOF terminates the stream cleanly.
func ReadRTPStream(r io.Reader, segment string, cfg RTPConfig) ([]Sample, *SourceReport, error) {
	rb := NewH264Reassembler(segment, cfg)
	var off int64
	lenBuf := make([]byte, 2)
	for {
		if _, err := io.ReadFull(r, lenBuf); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				rb.faults.Add(media.NewFault(media.KindTruncated, segment, 1, off,
					"capture length prefix truncated at EOF"))
				break
			}
			return nil, nil, media.NewFault(media.KindIO, segment, 1, off, "%s", err)
		}
		n := int(lenBuf[0])<<8 | int(lenBuf[1])
		if n <= 0 || n > 65535 {
			rb.faults.Add(media.NewFault(media.KindBadLength, segment, 1, off,
				"RTP datagram length %d out of range", n))
			break
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			rb.faults.Add(media.NewFault(media.KindTruncated, segment, 1, off, "datagram body truncated: %s", err))
			break
		}
		_ = rb.ProcessPacket(buf, off)
		off += int64(2 + n)
	}
	samples, report := rb.Finish()
	return samples, report, nil
}
