package demux

import (
	"encoding/binary"

	"mediaproc/internal/ebml"
	"mediaproc/internal/model"
)

func (p *Parser) parseBlockGroup(group ebml.Element, body []byte) (*Block, error) {
	var blockBody []byte
	var blockEl ebml.Element
	err := eachChild(body, func(el ebml.Element, b []byte) error {
		if el.ID == ebml.IDBlock {
			blockEl = el
			blockBody = b
		}
		return nil
	})
	if err != nil {
		return nil, model.NewError(model.PhaseDemux, group.Offset, true, "invalid BlockGroup", err)
	}
	if blockBody == nil {
		return nil, model.NewError(model.PhaseDemux, group.Offset, true, "BlockGroup has no Block", nil)
	}
	return p.decodeBlock(blockEl, blockBody, false)
}

func (p *Parser) parseBlock(el ebml.Element, simple bool) (*Block, error) {
	body, err := p.r.ReadPayload(el, p.cfg.MaxBlockPayload)
	if err != nil {
		return nil, err
	}
	return p.decodeBlock(el, body, simple)
}

func (p *Parser) decodeBlock(el ebml.Element, body []byte, simple bool) (*Block, error) {
	if len(body) < 4 {
		return nil, model.NewError(model.PhaseDemux, el.Offset, true, "truncated block header", nil)
	}
	v, n, err := ebml.DecodeVINT(body, false)
	if err != nil || n < 1 || n+3 > len(body) {
		return nil, model.NewError(model.PhaseDemux, el.Offset, true, "invalid block track VINT", err)
	}
	trackNumber := v.Value
	rel := int16(binary.BigEndian.Uint16(body[n : n+2]))
	flags := body[n+2]
	lacing := flags & ebml.LacingMask
	frames, err := parseLacedFrames(body, n+3, lacing)
	if err != nil {
		return nil, model.NewError(model.PhaseDemux, el.Offset, true, "invalid block lacing", err)
	}

	scale := int64(p.Metadata.TimecodeScaleNS)
	ts := p.cluster.timeNS + model.Nanoseconds(int64(rel)*scale)
	if last, ok := p.last[trackNumber]; ok && ts < last {
		return nil, model.NewError(model.PhaseDemux, el.Offset, false,
			"non-monotonic timestamp for track", nil)
	}
	p.last[trackNumber] = ts
	p.blockIndex++

	b := &Block{
		TrackNumber:   trackNumber,
		Track:         p.Metadata.Tracks[trackNumber],
		Offset:        el.Offset,
		ClusterOffset: p.cluster.offset,
		Index:         p.blockIndex,
		TimeNS:        ts,
		RelativeMS:    rel,
		Keyframe:      simple && flags&0x80 != 0,
		Invisible:     flags&0x08 != 0,
		Lacing:        lacing,
		Frames:        frames,
	}
	if len(frames) > 0 {
		b.Data = frames[0]
	}
	if b.Track != nil {
		b.Track.TimeScale = p.Metadata.TimecodeScaleNS
	}
	return b, nil
}
