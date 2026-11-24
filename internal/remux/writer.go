// Package remux performs lossless re-packaging: media payloads are copied
// byte-for-byte, packet order and timestamp relationships are preserved, and
// per-segment provenance (input ranges, packet count, digest) is recorded.
package remux

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"sync"

	"mediadeliver/internal/model"
)

// SegmentWriter streams one output segment into a container while hashing
// every copied payload. It never re-encodes media.
type SegmentWriter struct {
	mu sync.Mutex

	index      int
	outputFile string
	lim        model.Limits
	w          io.Writer
	hasher     hash.Hash

	packetCount int64
	bytes       int64
	closed      bool

	// per-source-range accounting
	ranges   []model.SourceRange
	curRange *model.SourceRange
	mappings []model.TrackMapping

	// last (track) seq to keep ranges contiguous
	lastTrack map[uint32]int64
}

// NewSegmentWriter opens a segment over w. mappings are stored verbatim in the
// provenance record; they are not used to alter payloads.
func NewSegmentWriter(index int, outputFile string, mappings []model.TrackMapping,
	w io.Writer, lim model.Limits) *SegmentWriter {
	cp := make([]model.TrackMapping, len(mappings))
	copy(cp, mappings)
	return &SegmentWriter{
		index: index, outputFile: outputFile, lim: lim, w: w,
		hasher:    sha256.New(),
		mappings:  cp,
		lastTrack: map[uint32]int64{},
	}
}

// WritePacket copies one packet payload unchanged. Order is whatever order the
// pipeline supplies (stable per track); timestamps are carried in the packet
// and re-emitted in the index, not rewritten here.
func (s *SegmentWriter) WritePacket(ctx context.Context, p *model.Packet) error {
	if err := ctx.Err(); err != nil {
		return model.Wrap(model.KindCancelled, "remux", "write packet", p.Seq,
			"cancelled before write", err)
	}
	if len(p.Payload) > s.lim.MaxPacketBytes {
		return model.At(model.KindLimitExceeded, "remux", "write packet", p.Seq,
			"oversized packet rejected at write stage")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return model.New(model.KindIO, "remux", "write packet", "write after close")
	}

	// Update provenance range for this source track.
	s.extendRange(p)

	n, err := s.w.Write(p.Payload)
	if err != nil {
		return model.Wrap(model.KindIO, "remux", "write payload", p.Seq,
			"output write failed", err)
	}
	if n != len(p.Payload) {
		return model.At(model.KindIO, "remux", "write payload", p.Seq,
			fmt.Sprintf("short write: %d of %d bytes", n, len(p.Payload)))
	}
	s.hasher.Write(p.Payload)
	s.bytes += int64(n)
	s.packetCount++
	s.lastTrack[p.TrackID] = p.Seq
	return nil
}

// extendRange maintains an inclusive [firstSeq,lastSeq] per source track.
func (s *SegmentWriter) extendRange(p *model.Packet) {
	for i := range s.ranges {
		r := &s.ranges[i]
		if r.MaterialID == p.SourceID && r.TrackID == p.TrackID {
			if p.Seq < r.FirstSeq {
				r.FirstSeq = p.Seq
			}
			if p.Seq > r.LastSeq {
				r.LastSeq = p.Seq
			}
			if p.DTS < r.StartDTS {
				r.StartDTS = p.DTS
			}
			if p.DTS > r.EndDTS {
				r.EndDTS = p.DTS
			}
			r.Packets++
			return
		}
	}
	s.ranges = append(s.ranges, model.SourceRange{
		MaterialID: p.SourceID, TrackID: p.TrackID,
		FirstSeq: p.Seq, LastSeq: p.Seq,
		StartDTS: p.DTS, EndDTS: p.DTS, Timescale: p.Timescale, Packets: 1,
	})
}

// Close finalizes the segment and returns its provenance record. It is safe to
// call once; a write/seek error propagates as KindIO.
func (s *SegmentWriter) Close() (*model.SegmentRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, model.New(model.KindIO, "remux", "close", "double close")
	}
	if closer, ok := s.w.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			return nil, model.Wrap(model.KindIO, "remux", "close", -1,
				"output close failed", err)
		}
	}
	s.closed = true
	ranges := make([]model.SourceRange, len(s.ranges))
	copy(ranges, s.ranges)
	return &model.SegmentRecord{
		Index:       s.index,
		OutputFile:  s.outputFile,
		Ranges:      ranges,
		Mappings:    s.mappings,
		PacketCount: s.packetCount,
		SHA256Hex:   hex.EncodeToString(s.hasher.Sum(nil)),
		Bytes:       s.bytes,
	}, nil
}

// PacketCount exposes the running count for metrics.
func (s *SegmentWriter) PacketCount() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.packetCount
}
