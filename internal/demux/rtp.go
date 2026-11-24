package demux

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/pion/rtp"

	"mediadeliver/internal/model"
)

// RTPReader demuxes a length-prefixed RTP capture. Frame layout:
//
//	uint32 big-endian length N, followed by N bytes of one RTP packet.
//
// A truncated length prefix or an RTP parse failure is reported as
// KindCorruptMedia carrying the 0-based packet index (first error position),
// which propagates unchanged into the index/remux layers.
type RTPReader struct {
	sourceID  string
	trackID   uint32
	kind      model.TrackKind
	codec     string
	timescale int64
	limits    model.Limits
	r         io.Reader

	haveBase bool
	baseTS   uint32 // first RTP timestamp; non-zero origin is preserved
	wrapExt  uint64 // accumulated 32-bit rollover extension
	prevExt  int64  // previous extended timestamp
	seq      int64
	packets  int64
	firstDTS int64
	lastDTS  int64
}

// NewRTPReader builds a demuxer for one RTP-backed source track.
func NewRTPReader(sourceID string, trackID uint32, kind model.TrackKind, codec string,
	timescale int64, r io.Reader, limits model.Limits) *RTPReader {
	return &RTPReader{
		sourceID: sourceID, trackID: trackID, kind: kind, codec: codec,
		timescale: timescale, r: r, limits: limits,
	}
}

// TrackInfo returns the descriptor; valid after the stream has been read.
func (x *RTPReader) TrackInfo() model.TrackInfo {
	return model.TrackInfo{
		ID: x.trackID, Kind: x.kind, Codec: x.codec, Timescale: x.timescale,
		Packets: x.packets, FirstDTS: x.firstDTS, LastDTS: x.lastDTS,
	}
}

// ReadPacket returns the next unified packet or io.EOF at end of capture.
func (x *RTPReader) ReadPacket(ctx context.Context) (*model.Packet, error) {
	if err := ctx.Err(); err != nil {
		return nil, model.Wrap(model.KindCancelled, "demux", "read rtp", x.seq,
			"cancelled before next packet", err)
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(x.r, lenBuf[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, io.EOF // clean end: no length prefix at all
		}
		return nil, model.Wrap(model.KindIO, "demux", "read rtp length", x.seq,
			"cannot read length prefix", err)
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n == 0 {
		return nil, model.At(model.KindCorruptMedia, "demux", "read rtp", x.seq,
			"zero-length RTP packet")
	}
	if int(n) > x.limits.MaxPacketBytes {
		return nil, model.At(model.KindLimitExceeded, "demux", "read rtp", x.seq,
			fmt.Sprintf("rtp packet %d bytes exceeds limit %d", n, x.limits.MaxPacketBytes))
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(x.r, buf); err != nil {
		// Truncated packet body: keep the packet index as first error position.
		return nil, model.Wrap(model.KindCorruptMedia, "demux", "read rtp", x.seq,
			fmt.Sprintf("truncated RTP packet: declared %d bytes", n), err)
	}
	var pkt rtp.Packet
	if err := pkt.Unmarshal(buf); err != nil {
		return nil, model.Wrap(model.KindCorruptMedia, "demux", "parse rtp", x.seq,
			"invalid RTP packet", err)
	}

	if !x.haveBase {
		x.haveBase = true
		x.baseTS = pkt.Timestamp
	}
	// Detect 32-bit timestamp rollover and extend to 64 bits. Different time
	// bases are never combined; the origin stays the raw first timestamp.
	if x.packets > 0 && pkt.Timestamp < uint32(x.prevExt) {
		x.wrapExt += 1 << 32
	}
	extTS := int64(x.wrapExt) + int64(pkt.Timestamp)

	dur := int64(0)
	if x.packets > 0 {
		dur = extTS - x.prevExt
	}
	x.prevExt = extTS

	if x.packets == 0 {
		x.firstDTS = extTS
	}
	x.lastDTS = extTS

	out := &model.Packet{
		SourceID: x.sourceID, TrackID: x.trackID, Kind: x.kind, Codec: x.codec,
		Seq: x.seq, DTS: extTS, PTS: extTS, Duration: dur, Timescale: x.timescale,
		// The marker bit is the only sync hint in raw RTP; ordinary payloads
		// are never reported as addressable cut points.
		Keyframe: pkt.Marker,
		Payload:  append([]byte(nil), pkt.Payload...),
	}
	x.seq++
	x.packets++
	return out, nil
}

// ReadAll drains the capture into a slice, stopping at the first corruption.
func (x *RTPReader) ReadAll(ctx context.Context) ([]*model.Packet, error) {
	var out []*model.Packet
	for {
		p, err := x.ReadPacket(ctx)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, p)
	}
}
