package demux

import (
	"io"

	"mediaproc/internal/ebml"
	"mediaproc/internal/model"
)

// Recover seeks to and validates the next Cluster boundary after a
// recoverable fault. The original fault offset is preserved and augmented
// with RecoveryOffset.
func (p *Parser) Recover(cause error) (int64, error) {
	fe, ok := model.AsError(cause)
	if !ok || !fe.Recoverable {
		return 0, cause
	}

	// Start one byte after the reported fault so the damaged header cannot be
	// selected again.
	if fe.Offset >= 0 {
		if err := p.r.Seek(fe.Offset + 1); err != nil {
			return 0, err
		}
	}

	for attempt := 0; attempt < 16; attempt++ {
		candidate, err := p.r.FindNextElement(ebml.IDCluster, p.cfg.RecoveryScan)
		if err != nil {
			return 0, err
		}
		if verr := p.validateCluster(candidate); verr == nil {
			if err := p.r.Seek(candidate.PayloadOffset()); err != nil {
				return 0, err
			}
			p.startCluster(candidate)
			p.cluster.timeNS = 0
			fe.RecoveryOffset = candidate.Offset
			return candidate.Offset, nil
		}
		_ = p.r.Seek(candidate.Offset + 1)
	}
	return 0, model.NewError(model.PhaseDemux, fe.Offset, false,
		"too many invalid Cluster candidates during recovery", cause)
}

// validateCluster performs a semantic boundary check: a Timecode and at least
// one structurally parseable block must be present.
func (p *Parser) validateCluster(cl ebml.Element) error {
	if err := p.r.Seek(cl.PayloadOffset()); err != nil {
		return err
	}
	foundTimecode := false
	foundBlock := false

	for i := 0; i < 64; i++ {
		pos := p.r.Offset()
		if !cl.Unknown && pos >= cl.End() {
			break
		}
		child, err := p.r.ReadElementHeader()
		if err != nil {
			return err
		}
		if child.ID == ebml.IDCluster || isUpperElement(child.ID) {
			return io.ErrUnexpectedEOF
		}
		if !cl.Unknown && child.End() > cl.End() {
			return io.ErrUnexpectedEOF
		}

		switch child.ID {
		case ebml.IDTimecode:
			body, err := p.r.ReadPayload(child, 16)
			if err != nil || len(body) == 0 || len(body) > 8 {
				return io.ErrUnexpectedEOF
			}
			foundTimecode = true
		case ebml.IDSimpleBlock:
			body, err := p.r.ReadPayload(child, p.cfg.MaxBlockPayload)
			if err != nil || len(body) < 4 {
				return io.ErrUnexpectedEOF
			}
			v, n, verr := ebml.DecodeVINT(body, false)
			if verr != nil || n+3 > len(body) || v.Value == 0 {
				return io.ErrUnexpectedEOF
			}
			lacing := body[n+2] & ebml.LacingMask
			if _, lerr := parseLacedFrames(body, n+3, lacing); lerr != nil {
				return io.ErrUnexpectedEOF
			}
			foundBlock = true
		case ebml.IDBlockGroup:
			body, err := p.r.ReadPayload(child, p.cfg.MaxBlockPayload)
			if err != nil {
				return io.ErrUnexpectedEOF
			}
			ok := false
			_ = eachChild(body, func(bh ebml.Element, bb []byte) error {
				if bh.ID == ebml.IDBlock && len(bb) >= 4 {
					ok = true
				}
				return nil
			})
			if !ok {
				return io.ErrUnexpectedEOF
			}
			foundBlock = true
		default:
			if child.Unknown {
				return io.ErrUnexpectedEOF
			}
			if err := p.r.SeekToEnd(child); err != nil {
				return err
			}
		}
		if foundTimecode && foundBlock {
			return nil
		}
	}
	if !foundTimecode || !foundBlock {
		return io.ErrUnexpectedEOF
	}
	return nil
}
