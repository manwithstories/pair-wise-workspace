package demux

import (
	"io"

	"mediaproc/internal/ebml"
	"mediaproc/internal/model"
)

// Block is one decoded and timestamped Matroska block.
type Block struct {
	TrackNumber   uint64
	Track         *Track
	Offset        int64
	ClusterOffset int64
	Index         int
	TimeNS        model.Nanoseconds
	RelativeMS    int16
	Keyframe      bool
	Invisible     bool
	Lacing        byte
	Frames        [][]byte
	Data          []byte
}

// Config bounds metadata and block buffering.
type Config struct {
	MaxMetadataPayload int64
	MaxBlockPayload    int64
	RecoveryScan       int64
}

// DefaultConfig returns small deterministic buffers suitable for the demo.
func DefaultConfig() Config {
	return Config{
		MaxMetadataPayload: 1 << 20,
		MaxBlockPayload:    256 << 10,
		RecoveryScan:       ebml.DefaultMaxScan,
	}
}

type clusterState struct {
	el     ebml.Element
	end    int64
	timeNS model.Nanoseconds
	offset int64
}

// Parser is a streaming Matroska/WebM block parser.
type Parser struct {
	r        *ebml.Reader
	cfg      Config
	Metadata *Metadata

	segmentEnd int64
	cluster    *clusterState
	last       map[uint64]model.Nanoseconds
	blockIndex int
}

// NewParser creates a Parser over a seekable stream. Seeking is only used for
// bounded corruption recovery; normal parsing is sequential.
func NewParser(rs io.ReadSeeker, cfg Config) (*Parser, error) {
	r, err := ebml.NewReader(rs)
	if err != nil {
		return nil, err
	}
	if cfg.MaxMetadataPayload == 0 {
		cfg.MaxMetadataPayload = 1 << 20
	}
	if cfg.MaxBlockPayload == 0 {
		cfg.MaxBlockPayload = 256 << 10
	}
	if cfg.RecoveryScan == 0 {
		cfg.RecoveryScan = ebml.DefaultMaxScan
	}
	return &Parser{
		r:        r,
		cfg:      cfg,
		Metadata: DefaultMetadata(),
		last:     map[uint64]model.Nanoseconds{},
	}, nil
}

// Offset returns the parser's current source offset.
func (p *Parser) Offset() int64 { return p.r.Offset() }

// ReadMetadata parses EBML/Info/Tracks and stops at the first Cluster header.
func (p *Parser) ReadMetadata() (*Metadata, error) {
	for {
		el, err := p.r.ReadElementHeader()
		if err != nil {
			return nil, err
		}
		if el.ID != ebml.IDSegment {
			if err := p.r.SeekToEnd(el); err != nil {
				return nil, err
			}
			continue
		}

		if el.Unknown {
			p.segmentEnd = -1
		} else {
			p.segmentEnd = el.End()
		}
		meta := DefaultMetadata()
		p.Metadata = meta
		if err := p.parseSegmentHeaderChildren(); err != nil {
			return nil, err
		}
		return meta, nil
	}
}

func (p *Parser) parseSegmentHeaderChildren() error {
	for {
		el, err := p.r.ReadElementHeader()
		if err != nil {
			return err
		}
		switch el.ID {
		case ebml.IDInfo:
			body, err := p.r.ReadPayload(el, p.cfg.MaxMetadataPayload)
			if err != nil {
				return err
			}
			p.Metadata.TimecodeScaleNS = ParseInfo(body)
		case ebml.IDTracks:
			body, err := p.r.ReadPayload(el, p.cfg.MaxMetadataPayload)
			if err != nil {
				return err
			}
			p.Metadata.Tracks = ParseTracks(body)
		case ebml.IDCluster:
			p.startCluster(el)
			return nil
		default:
			if err := p.r.SeekToEnd(el); err != nil {
				return err
			}
		}
	}
}

func (p *Parser) startCluster(el ebml.Element) {
	cs := &clusterState{el: el, offset: el.Offset}
	if el.Unknown {
		cs.end = -1
	} else {
		cs.end = el.End()
	}
	p.cluster = cs
}
