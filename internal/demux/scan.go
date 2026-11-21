// Package demux walks a Matroska/WebM file and emits frames in file order.
// It is the only package that understands Matroska's document structure; it
// hands frames to the block decoder and never buffers an entire file.
package demux

import (
	"errors"
	"fmt"
	"io"
	"os"

	"archivecheck.local/archivecheck/internal/ebml"
	"archivecheck.local/archivecheck/internal/fault"
	"archivecheck.local/archivecheck/internal/track"
)

// Info summarises the document header.
type Info struct {
	DocType        string
	DocTypeVersion uint64
	TimestampScale int64
	Duration       float64
	MuxingApp      string
	WritingApp     string
	// SegmentDataOffset is the absolute offset of the first Segment payload.
	SegmentDataOffset int64
	// SegmentEnd is the absolute end of the Segment (input size for unknown).
	SegmentEnd int64
	// SegmentUnknown reports an unknown-size Segment.
	SegmentUnknown bool
	// SegmentTruncated reports that the input ended inside the Segment's
	// declared extent.
	SegmentTruncated bool
	// HasCues reports whether a Cues element was seen.
	HasCues bool
	// CueEntries is the number of CuePoint elements found.
	CueEntries int
}

// Sample is one emitted frame with its timing and source range.
type Sample struct {
	TrackNumber uint64
	// Timestamp is the absolute presentation timestamp in Segment ticks.
	Timestamp int64
	// Keyframe is the sync flag.
	Keyframe bool
	// Data aliases a reusable buffer valid only until the next callback call.
	Data []byte
	// SourceOffset is the absolute offset of Data[0] in the input.
	SourceOffset int64
	// BlockOffset is the absolute offset of the block payload header.
	BlockOffset int64
	// ClusterOffset is the absolute offset of the enclosing Cluster.
	ClusterOffset int64
	// FrameIndex is the frame's index within its laced block.
	FrameIndex int
	// Laced reports that the sample came from a multi-frame block.
	Laced bool
	// Discardable mirrors the Matroska discardable flag.
	Discardable bool
}

// TrackStats accumulates per-track counters during a scan.
type TrackStats struct {
	Blocks    int64
	Frames    int64
	Keyframes int64
	FirstTick int64
	LastTick  int64
	HaveTick  bool
}

// Observe records one sample.
func (s *TrackStats) Observe(sample Sample) {
	if sample.FrameIndex == 0 {
		s.Blocks++
	}
	s.Frames++
	if sample.Keyframe {
		s.Keyframes++
	}
	if !s.HaveTick {
		s.FirstTick, s.LastTick, s.HaveTick = sample.Timestamp, sample.Timestamp, true
		return
	}
	if sample.Timestamp < s.FirstTick {
		s.FirstTick = sample.Timestamp
	}
	if sample.Timestamp > s.LastTick {
		s.LastTick = sample.Timestamp
	}
}

// Result summarises a scan.
type Result struct {
	Info   Info
	Tracks []track.Source
	Stats  map[uint64]*TrackStats
	// BlockCount counts every block payload seen, including unknown-track
	// blocks that were counted but not delivered.
	BlockCount int64
	// UnknownTrackBlocks counts blocks that referenced an undeclared track.
	UnknownTrackBlocks int64
	// UnknownTrackNumbers lists the undeclared track numbers seen.
	UnknownTrackNumbers []uint64
	// Recovery collects damage that was skipped with provable boundaries.
	Recovery []fault.Skip
}

// Options configures a scan.
type Options struct {
	// Recover enables skipping of damaged regions whose boundaries can be
	// proven. When false any damage is fatal.
	Recover bool
	// MaxBlockBytes caps a single block payload. Memory use is bounded by
	// this value regardless of file size.
	MaxBlockBytes int64
	// MaxCodecPrivateBytes caps CodecPrivate retained per track.
	MaxCodecPrivateBytes int64
	// OnTrack is called once per TrackEntry as soon as it is parsed.
	OnTrack func(track.Source)
	// OnSample is called for every frame of every declared track.
	OnSample func(Sample) error
}

// DefaultOptions returns conservative memory bounds.
func DefaultOptions() Options {
	return Options{
		MaxBlockBytes:        16 << 20,
		MaxCodecPrivateBytes: 8 << 20,
	}
}

// Scan parses path. The callback returning an error stops the scan and the
// error is returned unchanged (so cancellation propagates).
func Scan(path string, opts Options, report *Result) error {
	f, err := os.Open(path)
	if err != nil {
		return fault.New(fault.StageOpen, err)
	}
	defer f.Close()
	return ScanReader(f, opts, report)
}

// ScanReader parses r. It requires a seekable source.
func ScanReader(r io.ReadSeeker, opts Options, report *Result) error {
	if report == nil {
		return errors.New("demux: nil result")
	}
	if opts.MaxBlockBytes <= 0 {
		return errors.New("demux: MaxBlockBytes must be positive")
	}
	if opts.MaxCodecPrivateBytes <= 0 {
		opts.MaxCodecPrivateBytes = 8 << 20
	}
	s := &scanner{
		r:      ebml.NewReader(r),
		opts:   opts,
		result: report,
	}
	s.r.AllowTruncated = opts.Recover
	report.Stats = map[uint64]*TrackStats{}
	return s.run()
}

type scanner struct {
	r      *ebml.Reader
	opts   Options
	result *Result
	size   int64
	// clusterTS is the current Cluster timestamp.
	clusterTS int64
	// clusterOffset is the current Cluster start offset.
	clusterOffset int64
}

func (s *scanner) run() error {
	size, err := s.r.SourceSize()
	if err != nil {
		return fault.New(fault.StageOpen, err)
	}
	if size == 0 {
		return fault.AtElement(fault.StageParse, 0, "EBML", fault.ErrTruncated)
	}
	s.size = size
	s.r.SetSourceSize(size)

	// EBML document header.
	if err := s.readEBMLHeader(); err != nil {
		return err
	}

	// Find the Segment; SkipHead/Void may precede it.
	var seg ebml.Element
	for {
		e, err := s.r.Next(size)
		if err != nil {
			return err
		}
		switch e.ID {
		case ebml.IDSegment:
			seg = e
		case ebml.IDVoid:
			if err := s.r.Skip(e); err != nil {
				return err
			}
			continue
		default:
			if err := s.r.Skip(e); err != nil {
				return err
			}
			continue
		}
		break
	}
	s.result.Info.SegmentDataOffset = seg.DataOffset
	s.result.Info.SegmentUnknown = seg.UnknownSize
	segEnd := seg.End
	if seg.UnknownSize {
		segEnd = size
	}
	if seg.Truncated {
		// The declared Segment size crossed the end of input. Recovery mode
		// clamps it to the bytes actually present; the precise damaged range
		// is recorded when a Cluster tail is skipped.
		s.result.Info.SegmentTruncated = true
	}
	s.result.Info.SegmentEnd = segEnd
	if seg.DataOffset > size {
		return fault.AtElement(fault.StageParse, seg.Offset, "Segment", fault.ErrTruncated)
	}
	if err := s.walkSegment(seg.DataOffset, segEnd); err != nil {
		return err
	}
	return nil
}

func (s *scanner) readEBMLHeader() error {
	e, err := s.r.Next(s.size)
	if err != nil {
		return err
	}
	if e.ID != ebml.IDEBML {
		return fault.AtElement(fault.StageParse, e.Offset, e.Name(), fmt.Errorf("%w: expected EBML header", fault.ErrMalformed))
	}
	if e.UnknownSize {
		return fault.AtElement(fault.StageParse, e.Offset, e.Name(), fault.ErrMalformed)
	}
	end := e.End
	for s.r.Offset() < end {
		c, err := s.r.Next(end)
		if err != nil {
			return err
		}
		switch c.ID {
		case ebml.IDDocType:
			b, err := s.r.ReadSmall(c, 32)
			if err != nil {
				return err
			}
			s.result.Info.DocType = string(b)
		case ebml.IDDocTypeVersion:
			v, err := s.r.ReadUint(c)
			if err != nil {
				return err
			}
			s.result.Info.DocTypeVersion = v
		default:
			if err := s.r.Skip(c); err != nil {
				return err
			}
		}
	}
	if s.result.Info.DocType == "" {
		s.result.Info.DocType = "matroska"
	}
	switch s.result.Info.DocType {
	case "matroska", "webm":
	default:
		return fault.AtElement(fault.StageParse, e.Offset, "DocType",
			fmt.Errorf("%w: unsupported DocType %q", fault.ErrMalformed, s.result.Info.DocType))
	}
	return s.r.SeekTo(end)
}

func (s *scanner) walkSegment(start, end int64) error {
	if err := s.r.SeekTo(start); err != nil {
		return err
	}
	for s.r.Offset() < end {
		e, err := s.r.Next(end)
		if err != nil {
			return err
		}
		switch e.ID {
		case ebml.IDInfo:
			if err := s.readInfo(e); err != nil {
				return err
			}
		case ebml.IDTracks:
			if err := s.readTracks(e); err != nil {
				return err
			}
		case ebml.IDCluster:
			clusterEnd := e.End
			if e.UnknownSize {
				clusterEnd = end
			}
			if err := s.readCluster(e, clusterEnd); err != nil {
				if s.opts.Recover {
					recovered, rerr := s.recoverCluster(err, clusterEnd)
					if rerr != nil {
						return rerr
					}
					if recovered {
						continue
					}
				}
				return err
			}
		case ebml.IDCues:
			if err := s.readCues(e); err != nil {
				return err
			}
		case ebml.IDSeekHead, ebml.IDTags, ebml.IDChapters, ebml.IDAttachments, ebml.IDVoid, ebml.IDCRC32:
			if err := s.r.Skip(e); err != nil {
				return err
			}
		default:
			if e.UnknownSize {
				// Unknown-size non-container elements are not part of the
				// supported subset; skip to the parent boundary.
				if err := s.r.SeekTo(end); err != nil {
					return err
				}
				continue
			}
			if err := s.r.Skip(e); err != nil {
				return err
			}
		}
	}
	return nil
}
