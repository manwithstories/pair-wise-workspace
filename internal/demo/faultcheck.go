// Package demo contains acceptance workflow checks reusable by the CLI.
package demo

import (
	"errors"

	"mediad/internal/codec"
	"mediad/internal/fixture"
	"mediad/internal/ingest"
	"mediad/internal/mediaerr"
	"mediad/internal/transport"
)

// FaultCheckResult summarizes successful localization of every injected fault.
type FaultCheckResult struct {
	FirstBadOffset     int64
	OffsetErrorPackets int
	ContinuityGaps     int
	RetainedPSIVersion uint8
	RTPDuplicates      int
}

// VerifyFaults asserts all deterministic fault samples are located/handled.
func VerifyFaults(fb *fixture.FaultBundle) (FaultCheckResult, error) {
	out := FaultCheckResult{FirstBadOffset: -1}

	_, damageReport, err := ingest.ScanBytes(fb.DamageTS)
	if err != nil {
		return out, err
	}
	if damageReport.FirstBadOffset != fb.DamageOffset {
		return out, mediaerr.New(mediaerr.KindInvalidPacket, damageReport.FirstBadOffset, 0, false,
			"damage first_bad_offset=%d want %d", damageReport.FirstBadOffset, fb.DamageOffset)
	}
	out.FirstBadOffset = damageReport.FirstBadOffset
	out.OffsetErrorPackets = int((damageReport.FirstBadOffset - fb.DamageOffset) / ingest.PacketSize)
	if out.OffsetErrorPackets != 0 {
		return out, mediaerr.New(mediaerr.KindInvalidPacket, out.FirstBadOffset, 0, false,
			"offset error packets=%d", out.OffsetErrorPackets)
	}

	ccScanner := ingest.NewScanner()
	if _, err := ccScanner.Scan(fb.CCJump); err != nil {
		return out, err
	}
	var videoGaps int
	for _, g := range ccScanner.Report().Gaps {
		if g.PID == fixture.VideoPID {
			videoGaps++
		}
	}
	if videoGaps != 1 {
		return out, mediaerr.New(mediaerr.KindInvalidPacket, -1, fixture.VideoPID, false,
			"video continuity gaps=%d want 1", videoGaps)
	}
	out.ContinuityGaps = videoGaps

	psiScanner := ingest.NewScanner()
	if _, err := psiScanner.Scan(fb.BadCRCPSI); err != nil {
		return out, err
	}
	pmt := psiScanner.Tables.PMTs[fixture.PMTPID]
	if pmt == nil || pmt.Version != 0 || psiScanner.Report().PSIRejected == 0 {
		return out, mediaerr.New(mediaerr.KindChecksumMismatch, -1, fixture.PMTPID, false,
			"bad PSI candidate did not retain version 0")
	}
	out.RetainedPSIVersion = pmt.Version

	if _, _, err := ingest.ScanBytes(fb.Truncated); err == nil {
		return out, mediaerr.New(mediaerr.KindTruncated, -1, 0, false, "truncated sample did not return an error")
	} else {
		var se *mediaerr.Error
		if !errors.As(err, &se) || se.Kind != mediaerr.KindTruncated {
			return out, err
		}
	}

	if _, _, err := ingest.ScanBytes(fb.PCRWrap); err != nil {
		return out, err
	}
	if err := verifyCorruptCodec(fb.CorruptCodec); err != nil {
		return out, err
	}
	if err := verifyNegotiationErrors(); err != nil {
		return out, err
	}
	dups, err := verifyRTPFaults(fb)
	if err != nil {
		return out, err
	}
	out.RTPDuplicates = dups
	return out, nil
}

func verifyCorruptCodec(ts []byte) error {
	recs, _, err := ingest.ScanBytes(ts)
	if err != nil {
		return err
	}
	reg := codec.NewDefaultRegistry()
	parser, err := reg.Negotiate(codec.CodecH264, 1)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if rec.PID != fixture.VideoPID {
			continue
		}
		_, perr := parser.Parse(rec.Payload, codec.ParseContext{PID: rec.PID, Offset: rec.Offset})
		if perr == nil {
			return mediaerr.New(mediaerr.KindCorruptPayload, rec.Offset, rec.PID, false,
				"corrupt H.264 payload was accepted")
		}
		var se *mediaerr.Error
		if !errors.As(perr, &se) || se.Kind != mediaerr.KindCorruptPayload || !se.Recoverable {
			return mediaerr.Wrap(mediaerr.KindCorruptPayload, rec.Offset, rec.PID, false, perr)
		}
		return nil
	}
	return mediaerr.New(mediaerr.KindCorruptPayload, -1, fixture.VideoPID, false, "no corrupt video record found")
}

func verifyNegotiationErrors() error {
	reg := codec.NewDefaultRegistry()
	if _, err := reg.Negotiate(codec.CodecH264, 99); err == nil {
		return mediaerr.New(mediaerr.KindUnknownVersion, -1, 0, false, "unknown parser version accepted")
	} else {
		var se *mediaerr.Error
		if !errors.As(err, &se) || se.Kind != mediaerr.KindUnknownVersion {
			return err
		}
	}
	if _, err := reg.Negotiate(codec.Codec("av1"), 1); err == nil {
		return mediaerr.New(mediaerr.KindUnsupportedCodec, -1, 0, false, "unsupported codec accepted")
	} else {
		var se *mediaerr.Error
		if !errors.As(err, &se) || se.Kind != mediaerr.KindUnsupportedCodec {
			return err
		}
	}
	return nil
}

func verifyRTPFaults(fb *fixture.FaultBundle) (int, error) {
	dep := transport.NewDepacketizer(90000, 32)
	var stream []byte
	for i, raw := range fb.RTP {
		payloads, _, err := dep.PushBytes(raw, fb.RTPArrivalNS[i])
		if err != nil {
			return 0, err
		}
		for _, p := range payloads {
			stream = append(stream, p...)
		}
	}
	for _, p := range dep.Flush() {
		stream = append(stream, p...)
	}
	if dep.Stats.Duplicates == 0 {
		return 0, mediaerr.New(mediaerr.KindInvalidPacket, -1, 0, false, "RTP duplicate was not detected")
	}
	if dep.Stats.Reordered == 0 {
		return 0, mediaerr.New(mediaerr.KindInvalidPacket, -1, 0, false, "RTP reorder was not recovered")
	}
	scanner := ingest.NewScanner()
	if _, err := scanner.Scan(stream); err != nil {
		return 0, err
	}
	return dep.Stats.Duplicates, nil
}
