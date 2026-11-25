// Package demux splits Matroska/WebM streams into compressed packets. It
// understands unknown-size Segments and Clusters, block groups, and all three
// lacing modes. It never decodes media data.
package demux

import (
	"bufio"
	"io"
	"math"

	"mediapipe/internal/ebml"
	"mediapipe/internal/media"
)

// Limits bounds a single demux run.
type Limits struct {
	EBML          ebml.Limits
	MaxTrackCount int
	// MaxPackets bounds how many packets a single input may yield.
	MaxPackets int
}

// DefaultLimits returns limits suited to broadcast ingest of a single title.
func DefaultLimits() Limits {
	return Limits{
		EBML:          ebml.DefaultLimits(),
		MaxTrackCount: 64,
		MaxPackets:    5_000_000,
	}
}

func (l Limits) normalized() Limits {
	if l.MaxTrackCount <= 0 {
		l.MaxTrackCount = 256
	}
	if l.MaxPackets <= 0 {
		l.MaxPackets = 50_000_000
	}
	if l.EBML.MaxDepth <= 0 {
		l.EBML = ebml.DefaultLimits()
	}
	return l
}

// ClusterInfo records one cluster's byte span and optional CRC-32 element so
// the audit stage can verify integrity without trusting the parse.
type ClusterInfo struct {
	Offset      int64
	PayloadFrom int64 // first byte of the payload
	PayloadTo   int64 // one past the last payload byte
	HasCRC      bool
	CRCValue    uint32
	CRCOffset   int64 // offset of the CRC-32 element header
	CRCFrom     int64 // first byte covered by the CRC (end of the CRC element)
	Packets     int
}

// Result is the outcome of demuxing one input file.
type Result struct {
	DocType        string
	Tracks         []media.Track
	Packets        []media.Packet
	TimestampScale uint64 // nanoseconds per Matroska tick (normally 1e6)
	Duration       media.Timestamp
	Clusters       []ClusterInfo
	UnknownSizes   int   // count of unknown-size elements (Segment and Clusters)
	Truncated      bool  // source ended inside announced media data
	Bytes          int64 // absolute offset of the final byte consumed
}

// ErrTruncated marks a tail that ends inside announced media data.
func ErrTruncated() error {
	return media.NewError("demux.matroska", media.ClassFatal, io.ErrUnexpectedEOF)
}

type trackSpec struct {
	track      media.Track
	flagLacing bool
}

type parser struct {
	r          *ebml.Reader
	res        Result
	limits     Limits
	tracks     map[uint64]*trackSpec
	trackOrder []uint64
	tick       int64 // nanoseconds per timestamp tick
	clusterTS  int64 // current cluster timestamp in nanoseconds
	seq        uint64
	depth      int
	pending    *ebml.Element
	curCluster *ClusterInfo
}

// Demux reads a complete Matroska/WebM file from r.
func Demux(r io.Reader, limits Limits) (*Result, error) {
	limits = limits.normalized()
	p := &parser{
		r:      ebml.NewReaderWithLimits(bufio.NewReaderSize(r, 1<<20), limits.EBML),
		limits: limits,
		tracks: make(map[uint64]*trackSpec),
	}
	err := p.run()
	p.res.Tracks = make([]media.Track, 0, len(p.trackOrder))
	for _, id := range p.trackOrder {
		p.res.Tracks = append(p.res.Tracks, p.tracks[id].track)
	}
	p.res.Bytes = p.pos()
	if err != nil {
		return &p.res, err
	}
	return &p.res, nil
}

// pos reports the logical read position, accounting for a stashed element
// header that has been consumed from the stream but not yet processed.
func (p *parser) pos() int64 {
	if p.pending != nil {
		return p.pending.Offset
	}
	return p.r.Offset()
}

// next returns the stashed element or reads a fresh header.
func (p *parser) next() (ebml.Element, error) {
	if p.pending != nil {
		el := *p.pending
		p.pending = nil
		return el, nil
	}
	return p.r.ReadElement()
}

func (p *parser) run() error {
	for {
		el, err := p.next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch el.ID {
		case ebml.IDEBML:
			if err := p.readEBMLHeader(el); err != nil {
				return err
			}
		case ebml.IDSegment:
			if el.Unknown {
				p.res.UnknownSizes++
			}
			if err := p.readSegment(el); err != nil {
				return err
			}
		case ebml.IDVoid, ebml.IDCRC32:
			if el.Unknown {
				return media.At("demux.matroska", el.Offset, media.ClassFatal,
					"unknown-size element %#x outside Segment", uint32(el.ID))
			}
			if err := p.r.Skip(el.Payload); err != nil {
				return err
			}
		case ebml.IDCluster:
			// Segments with unknown size may be followed by trailing
			// top-level clusters when a writer closes lazily; accept them.
			if el.Unknown {
				p.res.UnknownSizes++
			}
			if err := p.readCluster(el); err != nil {
				return err
			}
		default:
			if el.Unknown {
				return media.At("demux.matroska", el.Offset, media.ClassFatal,
					"unknown-size top-level element %#x", uint32(el.ID))
			}
			if err := p.r.Skip(el.Payload); err != nil {
				return err
			}
		}
	}
}

func (p *parser) readEBMLHeader(el ebml.Element) error {
	if el.Unknown {
		return media.At("demux.ebml", el.Offset, media.ClassFatal, "EBML header has unknown size")
	}
	return p.readChildren(el, func(child ebml.Element) error {
		if child.ID == ebml.IDDocType {
			s, err := p.r.ReadString(child.Payload)
			if err != nil {
				return err
			}
			if s != "matroska" && s != "webm" {
				return media.At("demux.ebml", child.Offset, media.ClassUnsupported,
					"unsupported DocType %q", s)
			}
			p.res.DocType = s
			return nil
		}
		return p.r.Skip(child.Payload)
	})
}

func (p *parser) readSegment(el ebml.Element) error {
	return p.readChildren(el, func(child ebml.Element) error {
		switch child.ID {
		case ebml.IDInfo:
			return p.readInfo(child)
		case ebml.IDTracks:
			return p.readTracks(child)
		case ebml.IDCluster:
			if child.Unknown {
				p.res.UnknownSizes++
			}
			return p.readCluster(child)
		default:
			return p.skipElement(child)
		}
	})
}

// skipElement advances past a non-media element.
func (p *parser) skipElement(el ebml.Element) error {
	if el.Unknown {
		return media.At("demux.skip", el.Offset, media.ClassFatal,
			"unknown-size element %#x is not a supported container", uint32(el.ID))
	}
	return p.r.Skip(el.Payload)
}

func (p *parser) readInfo(el ebml.Element) error {
	p.tick = int64(media.Millisecond) // Matroska default TimestampScale
	return p.readChildren(el, func(child ebml.Element) error {
		switch child.ID {
		case ebml.IDTimestampScale:
			v, err := readUint(p.r, child)
			if err != nil {
				return err
			}
			if v == 0 || v > math.MaxUint32 {
				return media.At("demux.info", child.Offset, media.ClassFatal,
					"invalid TimestampScale %d", v)
			}
			p.tick = int64(v)
		case ebml.IDDuration:
			f, err := readFloat(p.r, child)
			if err != nil {
				return err
			}
			p.res.Duration = scaleFloat(f, uint32(p.tick))
		default:
			return p.r.Skip(child.Payload)
		}
		return nil
	})
}

func (p *parser) readTracks(el ebml.Element) error {
	count := 0
	err := p.readChildren(el, func(child ebml.Element) error {
		if child.ID != ebml.IDTrackEntry {
			return p.r.Skip(child.Payload)
		}
		count++
		if count > p.limits.MaxTrackCount {
			return media.At("demux.tracks", child.Offset, media.ClassFatal,
				"track count exceeds limit %d", p.limits.MaxTrackCount)
		}
		return p.readTrackEntry(child)
	})
	if err == nil && len(p.trackOrder) == 0 {
		return media.At("demux.tracks", el.Offset, media.ClassFatal, "segment declares no tracks")
	}
	return err
}

func (p *parser) readTrackEntry(el ebml.Element) error {
	spec := &trackSpec{track: media.Track{Timescale: 1_000_000_000}}
	var haveNumber, haveType, haveCodec bool
	err := p.readChildren(el, func(child ebml.Element) error {
		switch child.ID {
		case ebml.IDTrackNumber:
			v, err := readUint(p.r, child)
			if err != nil {
				return err
			}
			if v == 0 {
				return media.At("demux.track", child.Offset, media.ClassFatal, "track number 0")
			}
			spec.track.ID, haveNumber = v, true
		case ebml.IDTrackUID:
			v, err := readUint(p.r, child)
			if err != nil {
				return err
			}
			spec.track.UID = v
		case ebml.IDTrackType:
			v, err := readUint(p.r, child)
			if err != nil {
				return err
			}
			spec.track.Type, haveType = media.TrackType(v), true
		case ebml.IDFlagLacing:
			v, err := readUint(p.r, child)
			if err != nil {
				return err
			}
			spec.flagLacing = v != 0
		case ebml.IDDefaultDuration:
			v, err := readUint(p.r, child)
			if err != nil {
				return err
			}
			spec.track.DefaultDuration = media.Timestamp(v)
		case ebml.IDLanguage:
			s, err := p.r.ReadString(child.Payload)
			if err != nil {
				return err
			}
			spec.track.Language = s
		case ebml.IDCodecID:
			s, err := p.r.ReadString(child.Payload)
			if err != nil {
				return err
			}
			spec.track.Codec, haveCodec = media.Codec(s), true
		case ebml.IDCodecPrivate:
			b, err := p.r.ReadBytes(child.Payload)
			if err != nil {
				return err
			}
			spec.track.CodecPrivate = b
		case ebml.IDVideo:
			return p.readVideo(child, &spec.track)
		case ebml.IDAudio:
			return p.readAudio(child, &spec.track)
		default:
			return p.r.Skip(child.Payload)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !haveNumber || !haveType || !haveCodec {
		return media.At("demux.track", el.Offset, media.ClassFatal,
			"track entry missing number/type/codec")
	}
	if _, dup := p.tracks[spec.track.ID]; dup {
		return media.At("demux.track", el.Offset, media.ClassFatal,
			"duplicate track number %d", spec.track.ID)
	}
	if spec.track.Language == "" {
		spec.track.Language = "und"
	}
	p.tracks[spec.track.ID] = spec
	p.trackOrder = append(p.trackOrder, spec.track.ID)
	return nil
}

func (p *parser) readVideo(el ebml.Element, t *media.Track) error {
	return p.readChildren(el, func(child ebml.Element) error {
		switch child.ID {
		case ebml.IDPixelWidth:
			v, err := readUint(p.r, child)
			if err != nil {
				return err
			}
			t.Width = int(v)
		case ebml.IDPixelHeight:
			v, err := readUint(p.r, child)
			if err != nil {
				return err
			}
			t.Height = int(v)
		default:
			return p.r.Skip(child.Payload)
		}
		return nil
	})
}

func (p *parser) readAudio(el ebml.Element, t *media.Track) error {
	return p.readChildren(el, func(child ebml.Element) error {
		switch child.ID {
		case ebml.IDChannels:
			v, err := readUint(p.r, child)
			if err != nil {
				return err
			}
			t.Channels = int(v)
		case ebml.IDSamplingFrequency:
			f, err := readFloat(p.r, child)
			if err != nil {
				return err
			}
			t.SampleRate = int(f)
		default:
			return p.r.Skip(child.Payload)
		}
		return nil
	})
}

// readChildren iterates the direct children of el. For unknown-size
// containers it stops when the next element cannot belong to el and stashes
// that element for the parent loop.
func (p *parser) readChildren(el ebml.Element, fn func(child ebml.Element) error) error {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > p.limits.EBML.MaxDepth {
		return media.At("demux.depth", el.Offset, media.ClassFatal,
			"element nesting deeper than limit %d", p.limits.EBML.MaxDepth)
	}
	parentEnd := el.End()
	for {
		start := p.pos()
		if parentEnd >= 0 && start >= parentEnd {
			if start > parentEnd {
				return media.At("demux.children", start, media.ClassFatal,
					"element %#x overruns its parent by %d bytes", uint32(el.ID), start-parentEnd)
			}
			return nil
		}
		child, err := p.next()
		if err == io.EOF {
			if parentEnd >= 0 {
				return media.At("demux.children", start, media.ClassFatal,
					"element %#x truncated: %d bytes missing", uint32(el.ID), parentEnd-start)
			}
			if el.Unknown && p.depth > 1 {
				// Unknown-size Cluster ended with the file; acceptable only
				// for the outermost Segment.
				return nil
			}
			return nil
		}
		if err != nil {
			return err
		}
		if el.Unknown && isBoundary(el.ID, child.ID) {
			p.pending = &child
			return nil
		}
		if parentEnd >= 0 && !child.Unknown && child.End() > parentEnd {
			return media.At("demux.children", child.Offset, media.ClassFatal,
				"element %#x length %d overruns parent %#x", uint32(child.ID), child.Payload, uint32(el.ID))
		}
		if err := fn(child); err != nil {
			return err
		}
	}
}

// isBoundary reports whether child cannot be a child of parent, which means an
// unknown-size parent has ended.
func isBoundary(parentID, childID ebml.ID) bool {
	switch parentID {
	case ebml.IDCluster:
		switch childID {
		case ebml.IDCluster, ebml.IDTracks, ebml.IDInfo, ebml.IDSegment,
			ebml.IDCues, ebml.IDTags, ebml.IDSeekHead:
			return true
		}
	case ebml.IDSegment:
		switch childID {
		case ebml.IDSegment:
			return true
		}
	}
	return false
}

func readUint(r *ebml.Reader, el ebml.Element) (uint64, error) {
	return r.ReadUint(int(el.Payload))
}

func readFloat(r *ebml.Reader, el ebml.Element) (float64, error) {
	return r.ReadFloat(int(el.Payload))
}

func scaleFloat(v float64, tick uint32) media.Timestamp {
	if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	ns := v * float64(tick)
	if ns >= math.MaxInt64 {
		return media.Timestamp(math.MaxInt64)
	}
	return media.Timestamp(ns)
}
