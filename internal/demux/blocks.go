package demux

import (
	"errors"
	"io"

	"mediaproc/internal/ebml"
	"mediaproc/internal/model"
)

var errEndCluster = errors.New("end of unknown/current cluster")

func (p *Parser) nextCluster() error {
	for {
		pos := p.r.Offset()
		if p.segmentEnd >= 0 && pos >= p.segmentEnd {
			return io.EOF
		}
		el, err := p.r.ReadElementHeader()
		if err == io.EOF {
			return io.EOF
		}
		if err != nil {
			return err
		}
		if el.ID == ebml.IDCluster {
			p.startCluster(el)
			return nil
		}
		// Top-level Segment metadata/Cues end an unknown-length Cluster.
		if !el.Unknown {
			if err := p.r.SeekToEnd(el); err != nil {
				return err
			}
			continue
		}
		return model.NewError(model.PhaseDemux, el.Offset, true,
			"unexpected unknown-length upper element", nil)
	}
}

// NextBlock returns blocks in file order. Interleaved tracks retain their own
// per-track order and are checked for timestamp monotonicity.
func (p *Parser) NextBlock() (*Block, error) {
	for {
		if p.cluster == nil {
			if err := p.nextCluster(); err != nil {
				return nil, err
			}
		}
		b, err := p.nextBlockInCluster()
		if errors.Is(err, errEndCluster) {
			p.cluster = nil
			continue
		}
		return b, err
	}
}

func (p *Parser) nextBlockInCluster() (*Block, error) {
	for {
		pos := p.r.Offset()
		if p.cluster.end >= 0 && pos >= p.cluster.end {
			return nil, errEndCluster
		}
		el, err := p.r.ReadElementHeader()
		if err == io.EOF {
			if p.cluster.end < 0 {
				return nil, errEndCluster
			}
			return nil, io.ErrUnexpectedEOF
		}
		if fe, ok := model.AsError(err); ok {
			return nil, fe
		}
		if err != nil {
			return nil, model.NewError(model.PhaseDemux, pos, true,
				"invalid EBML header", err)
		}

		if el.ID == ebml.IDCluster || isUpperElement(el.ID) {
			if p.cluster.end < 0 {
				if err := p.r.SeekStart(el.Offset); err != nil {
					return nil, err
				}
				return nil, errEndCluster
			}
		}
		if p.cluster.end >= 0 && el.End() > p.cluster.end {
			return nil, model.NewError(model.PhaseDemux, el.Offset, true,
				"block/element extends beyond Cluster boundary", nil)
		}

		switch el.ID {
		case ebml.IDTimecode:
			body, err := p.r.ReadPayload(el, 16)
			if err != nil {
				return nil, err
			}
			p.cluster.timeNS = model.FromTimecode(
				readUint(body), p.Metadata.TimecodeScaleNS)
		case ebml.IDSimpleBlock:
			return p.parseBlock(el, true)
		case ebml.IDBlockGroup:
			body, err := p.r.ReadPayload(el, p.cfg.MaxBlockPayload)
			if err != nil {
				return nil, err
			}
			return p.parseBlockGroup(el, body)
		default:
			if el.Unknown {
				return nil, model.NewError(model.PhaseDemux, el.Offset, true,
					"unknown-length unsupported cluster child", nil)
			}
			if err := p.r.SeekToEnd(el); err != nil {
				return nil, err
			}
		}
	}
}

func isUpperElement(id uint32) bool {
	switch id {
	case ebml.IDInfo, ebml.IDTracks, ebml.IDSeekHead, ebml.IDCues, ebml.IDTags,
		ebml.IDChapters, ebml.IDAttachments, ebml.IDSegment:
		return true
	default:
		return false
	}
}
