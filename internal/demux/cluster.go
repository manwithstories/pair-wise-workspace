package demux

import (
	"errors"
	"fmt"

	"archivecheck.local/archivecheck/internal/ebml"
	"archivecheck.local/archivecheck/internal/fault"
	"archivecheck.local/archivecheck/internal/mkv"
)

// readCluster iterates a Cluster's children and delivers its blocks. For an
// unknown-size Cluster, limit is the Segment end and the loop stops when a
// sibling element that may follow a Cluster is peeked; the reader is rewound
// to that element's first byte so the Segment loop can consume it.
func (s *scanner) readCluster(e ebml.Element, limit int64) error {
	if err := s.r.SeekTo(e.DataOffset); err != nil {
		return err
	}
	s.clusterOffset = e.Offset
	s.clusterTS = 0
	haveTS := false
	buf := make([]byte, 0, 64<<10)
	for s.r.Offset() < limit {
		childStart := s.r.Offset()
		c, err := s.r.Next(limit)
		if err != nil {
			if s.opts.Recover && errors.Is(err, fault.ErrTruncated) {
				// The input ends inside this Cluster. Every block header that
				// did parse belongs to a proven boundary, so the incomplete
				// tail can be skipped and recorded.
				s.result.Recovery = append(s.result.Recovery, fault.Skip{
					Start:  childStart,
					Length: limit - childStart,
					Reason: "input ends inside Cluster",
				})
				return s.r.SeekTo(limit)
			}
			return err
		}
		if c.Truncated {
			// A child (usually a SimpleBlock) crosses the end of input. No
			// frame from this byte onward can be trusted, so the tail is
			// skipped as one proven range and no index entry is created.
			s.result.Recovery = append(s.result.Recovery, fault.Skip{
				Start:  childStart,
				Length: limit - childStart,
				Reason: "input ends inside Cluster",
			})
			return s.r.SeekTo(limit)
		}
		if e.UnknownSize && c.ID != ebml.IDTimestamp && c.ID != ebml.IDSimpleBlock &&
			c.ID != ebml.IDBlockGroup && c.ID != ebml.IDBlock && c.ID != ebml.IDVoid &&
			c.ID != ebml.IDCRC32 && c.ID != ebml.IDDiscardPadding {
			// A sibling that the Matroska format permits at Segment level
			// terminates this unknown-size Cluster.
			return s.r.SeekTo(childStart)
		}
		switch c.ID {
		case ebml.IDTimestamp:
			v, err := s.r.ReadUint(c)
			if err != nil {
				return err
			}
			s.clusterTS = int64(v)
			haveTS = true
		case ebml.IDSimpleBlock:
			if !haveTS {
				return fault.AtElement(fault.StageDemux, c.Offset, "SimpleBlock",
					fmt.Errorf("%w: block before Cluster Timestamp", fault.ErrMalformed))
			}
			buf, err = s.readBlockPayload(c, buf)
			if err != nil {
				return err
			}
			b, err := mkv.ParseSimpleBlock(buf, c.DataOffset)
			if err != nil {
				return err
			}
			if err := s.emitBlock(b); err != nil {
				return err
			}
		case ebml.IDBlockGroup:
			if !haveTS {
				return fault.AtElement(fault.StageDemux, c.Offset, "BlockGroup",
					fmt.Errorf("%w: block before Cluster Timestamp", fault.ErrMalformed))
			}
			buf, err = s.readBlockGroup(c, buf)
			if err != nil {
				return err
			}
		default:
			if err := s.r.Skip(c); err != nil {
				return err
			}
		}
	}
	return nil
}

// readBlockGroup reads a BlockGroup, including the Keyframe and
// ReferenceBlock children that determine the sync flag, then emits the block.
func (s *scanner) readBlockGroup(e ebml.Element, buf []byte) ([]byte, error) {
	if e.UnknownSize {
		return buf, fault.AtElement(fault.StageDemux, e.Offset, "BlockGroup", fault.ErrMalformed)
	}
	var (
		haveKeyframe bool
		keyframe     bool
		haveBlock    bool
		blockOffset  int64
		blockData    []byte
	)
	for s.r.Offset() < e.End {
		c, err := s.r.Next(e.End)
		if err != nil {
			return buf, err
		}
		switch c.ID {
		case ebml.IDBlock:
			if haveBlock {
				return buf, fault.AtElement(fault.StageDemux, c.Offset, "BlockGroup",
					fmt.Errorf("%w: multiple Block elements", fault.ErrMalformed))
			}
			buf, err = s.readBlockPayload(c, buf)
			if err != nil {
				return buf, err
			}
			blockData = buf
			blockOffset = c.DataOffset
			haveBlock = true
		case ebml.IDKeyframe:
			v, err := s.r.ReadUint(c)
			if err != nil {
				return buf, err
			}
			keyframe = v != 0
			haveKeyframe = true
		case ebml.IDReferenceBlock:
			// Presence implies a non-keyframe, but only when Keyframe is
			// absent (older muxers).
			if _, err := s.r.ReadInt(c); err != nil {
				return buf, err
			}
			if !haveKeyframe {
				keyframe = false
				haveKeyframe = true
			}
		default:
			if err := s.r.Skip(c); err != nil {
				return buf, err
			}
		}
	}
	if !haveBlock {
		return buf, fault.AtElement(fault.StageDemux, e.Offset, "BlockGroup",
			fmt.Errorf("%w: BlockGroup without Block", fault.ErrMalformed))
	}
	if !haveKeyframe {
		// No Keyframe and no ReferenceBlock: treat as keyframe only for
		// single-frame audio, else as non-keyframe. The track mapper decides
		// the final stss membership from this flag.
		keyframe = false
	}
	b, err := mkv.ParseBlock(blockData, blockOffset, keyframe)
	if err != nil {
		return buf, err
	}
	if err := s.emitBlock(b); err != nil {
		return buf, err
	}
	return buf, nil
}

// readBlockPayload reads one block payload into buf, growing it up to the
// configured maximum. The buffer is reused across blocks to keep allocations
// bounded by MaxBlockBytes rather than by block count.
func (s *scanner) readBlockPayload(c ebml.Element, buf []byte) ([]byte, error) {
	if c.UnknownSize {
		return buf, fault.AtElement(fault.StageDemux, c.Offset, c.Name(), fault.ErrMalformed)
	}
	if c.Size > s.opts.MaxBlockBytes {
		return buf, fault.AtElement(fault.StageDemux, c.Offset, c.Name(),
			fmt.Errorf("%w: block of %d bytes exceeds limit %d", fault.ErrOversized, c.Size, s.opts.MaxBlockBytes))
	}
	if int64(cap(buf)) < c.Size {
		buf = make([]byte, c.Size)
	}
	buf = buf[:c.Size]
	if err := s.r.SeekTo(c.DataOffset); err != nil {
		return buf, err
	}
	if _, err := s.r.ReadFull(buf); err != nil {
		return buf, fault.AtElement(fault.StageDemux, c.Offset, c.Name(), err)
	}
	return buf, nil
}

func (s *scanner) emitBlock(b mkv.Block) error {
	s.result.BlockCount++
	declared := s.declared(b.TrackNumber)
	if !declared {
		s.result.UnknownTrackBlocks++
		if !contains(s.result.UnknownTrackNumbers, b.TrackNumber) {
			s.result.UnknownTrackNumbers = append(s.result.UnknownTrackNumbers, b.TrackNumber)
		}
		return nil
	}
	stats := s.result.Stats[b.TrackNumber]
	if stats == nil {
		stats = &TrackStats{}
		s.result.Stats[b.TrackNumber] = stats
	}
	laced := len(b.Frames) > 1
	for i, f := range b.Frames {
		sample := Sample{
			TrackNumber:   b.TrackNumber,
			Timestamp:     s.clusterTS + int64(b.RelativeTimestamp),
			Keyframe:      b.Keyframe,
			Data:          f.Data,
			SourceOffset:  f.SourceOffset,
			BlockOffset:   b.SourceOffset,
			ClusterOffset: s.clusterOffset,
			FrameIndex:    i,
			Laced:         laced,
			Discardable:   b.Discardable,
		}
		stats.Observe(sample)
		if s.opts.OnSample != nil {
			if err := s.opts.OnSample(sample); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *scanner) declared(n uint64) bool {
	for _, t := range s.result.Tracks {
		if t.Number == n {
			return true
		}
	}
	return false
}

func contains(list []uint64, v uint64) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// recoverCluster attempts to resume after a corrupt Cluster. Recovery is only
// allowed when the failing cluster has a provable end: the next Cluster or
// top-level element header parsed cleanly before the failure was discovered.
// In that case the bytes of the whole bad cluster are skipped and recorded.
func (s *scanner) recoverCluster(cause error, clusterEnd int64) (bool, error) {
	if clusterEnd <= 0 {
		return false, cause
	}
	var fe *fault.Error
	if !errors.As(cause, &fe) {
		return false, cause
	}
	if fe.Class() != fault.ClassCorrupt {
		return false, cause
	}
	if fe.Offset >= clusterEnd {
		// The failure is outside this cluster's proven extent; not recoverable
		// here.
		return false, cause
	}
	skipped := clusterEnd - s.clusterOffset
	if skipped <= 0 {
		return false, cause
	}
	if err := s.r.SeekTo(clusterEnd); err != nil {
		return false, err
	}
	s.result.Recovery = append(s.result.Recovery, fault.Skip{
		Start:  s.clusterOffset,
		Length: skipped,
		Reason: fmt.Sprintf("corrupt cluster: %v", fe.Err),
	})
	return true, nil
}
