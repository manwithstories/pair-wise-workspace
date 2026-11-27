package mp4

import (
	"encoding/binary"
	"io"

	"github.com/fashionweek/mediad/internal/clock"
	"github.com/fashionweek/mediad/internal/media"
)

// maxReorderWindows bounds how far a sample may be reordered from decode into
// presentation order. Normal B-frame structures stay below this; the PTS
// rollback corruption fixture pushes a sample far beyond it.
const maxReorderWindow = 16

func parseTrack(ra io.ReaderAt, trak Box, hdr *Header) (*TrackIndex, error) {
	mdia, ok, err := FindBox(ra, trak.Payload, trak.ContentEnd(), "mdia")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, media.New(media.PhaseIndex, media.KindTruncated, "", trak.Offset, "mdia box missing")
	}
	tr := &TrackIndex{}
	if err := parseMDHD(ra, mdia, tr); err != nil {
		return nil, err
	}
	if err := parseHDLR(ra, mdia, tr); err != nil {
		return nil, err
	}
	if edts, ok, err := FindBox(ra, trak.Payload, trak.ContentEnd(), "edts"); err != nil {
		return nil, err
	} else if ok {
		if err := parseEDTS(ra, edts, tr); err != nil {
			return nil, err
		}
	}
	minf, ok, err := FindBox(ra, mdia.Payload, mdia.ContentEnd(), "minf")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, media.New(media.PhaseIndex, media.KindTruncated, "", mdia.Offset, "minf box missing")
	}
	stbl, ok, err := FindBox(ra, minf.Payload, minf.ContentEnd(), "stbl")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, media.New(media.PhaseIndex, media.KindTruncated, "", minf.Offset, "stbl box missing")
	}
	if err := parseCodec(ra, stbl, tr); err != nil {
		return nil, err
	}
	if err := parseSampleTables(ra, stbl, tr, hdr); err != nil {
		return nil, err
	}
	return tr, nil
}

func parseMDHD(ra io.ReaderAt, mdia Box, tr *TrackIndex) error {
	b, ok, err := FindBox(ra, mdia.Payload, mdia.ContentEnd(), "mdhd")
	if err != nil {
		return err
	}
	if !ok {
		return media.New(media.PhaseIndex, media.KindTruncated, "", mdia.Offset, "mdhd box missing")
	}
	buf, err := ReadPayload(ra, b, 1<<20)
	if err != nil {
		return err
	}
	version, _, rest := FullBoxVersion(buf)
	at := 0
	var ts uint32
	var dur uint64
	if version == 1 {
		at += 16 // creation/modification
		if ts, at, err = readU32(rest, at, b.Payload); err != nil {
			return err
		}
		if dur, at, err = readU64(rest, at, b.Payload); err != nil {
			return err
		}
	} else {
		at += 8
		if ts, at, err = readU32(rest, at, b.Payload); err != nil {
			return err
		}
		var d32 uint32
		if d32, at, err = readU32(rest, at, b.Payload); err != nil {
			return err
		}
		dur = uint64(d32)
	}
	if ts == 0 {
		// First-byte offset of the timescale field within the mdhd payload.
		off := b.Payload + int64(at) - 4
		return media.New(media.PhaseIndex, media.KindBadTimebase, "", off,
			"track mdhd declares zero timescale (invalid media time base)")
	}
	tr.Timescale = ts
	d, err := clock.New(int64(dur), int64(ts))
	if err != nil {
		return media.New(media.PhaseIndex, media.KindOverflow, "", b.Payload,
			"track duration not representable").Wrap(err)
	}
	tr.Duration = d
	return nil
}

func parseHDLR(ra io.ReaderAt, mdia Box, tr *TrackIndex) error {
	b, ok, err := FindBox(ra, mdia.Payload, mdia.ContentEnd(), "hdlr")
	if err != nil {
		return err
	}
	if !ok {
		return media.New(media.PhaseIndex, media.KindTruncated, "", mdia.Offset, "hdlr box missing")
	}
	buf, err := ReadPayload(ra, b, 1<<20)
	if err != nil {
		return err
	}
	if len(buf) < 16 {
		return media.New(media.PhaseScan, media.KindTruncated, "", b.Payload, "hdlr too short")
	}
	// Full payload layout: version/flags(4) + pre_defined(4) + handler_type(4),
	// so handler_type ("vide"/"soun") sits at offset 8 within ReadPayload's
	// buffer — ReadPayload already includes the 4-byte full-box prefix.
	tr.Video = string(buf[8:12]) == "vide"
	return nil
}

func parseEDTS(ra io.ReaderAt, edts Box, tr *TrackIndex) error {
	elst, ok, err := FindBox(ra, edts.Payload, edts.ContentEnd(), "elst")
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	buf, err := ReadPayload(ra, elst, 4<<20)
	if err != nil {
		return err
	}
	version, _, rest := FullBoxVersion(buf)
	if len(rest) < 4 {
		return media.New(media.PhaseScan, media.KindTruncated, "", elst.Payload, "elst entry count missing")
	}
	n := binary.BigEndian.Uint32(rest[:4])
	at := 4
	den := int64(tr.Timescale)
	if den == 0 {
		return media.New(media.PhaseIndex, media.KindBadTimebase, "", elst.Payload,
			"edit list before valid timescale")
	}
	for i := uint32(0); i < n; i++ {
		var segDur, mediaTime int64
		if version == 1 {
			if at+20 > len(rest) {
				return media.New(media.PhaseScan, media.KindTruncated, "", elst.Payload+int64(at),
					"truncated v1 edit entry")
			}
			segDur = int64(binary.BigEndian.Uint64(rest[at:]))
			mediaTime = int64(binary.BigEndian.Uint64(rest[at+8:]))
			at += 20
		} else {
			if at+12 > len(rest) {
				return media.New(media.PhaseScan, media.KindTruncated, "", elst.Payload+int64(at),
					"truncated v0 edit entry")
			}
			segDur = int64(binary.BigEndian.Uint32(rest[at:]))
			mediaTime = int64(binary.BigEndian.Uint32(rest[at+4:]))
			at += 12
		}
		sd, err := clock.New(segDur, den)
		if err != nil {
			return media.New(media.PhaseIndex, media.KindOverflow, "", elst.Payload,
				"edit segment duration overflow").Wrap(err)
		}
		e := EditEntry{SegmentDuration: sd, Empty: mediaTime < 0}
		if !e.Empty {
			mt, err := clock.New(mediaTime, den)
			if err != nil {
				return media.New(media.PhaseIndex, media.KindOverflow, "", elst.Payload,
					"edit media time overflow").Wrap(err)
			}
			e.MediaTime = mt
		}
		tr.Edits = append(tr.Edits, e)
	}
	return nil
}

func parseCodec(ra io.ReaderAt, stbl Box, tr *TrackIndex) error {
	var stsd Box
	found := false
	err := Walk(ra, stbl.Payload, stbl.ContentEnd(), func(b Box) error {
		if b.Type == "stsd" {
			stsd, found = b, true
			return errStop
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return media.New(media.PhaseIndex, media.KindTruncated, "", stbl.Offset, "stsd box missing")
	}
	buf, err := ReadPayload(ra, stsd, 4<<20)
	if err != nil {
		return err
	}
	if len(buf) < 16 {
		return media.New(media.PhaseScan, media.KindTruncated, "", stsd.Payload, "stsd too short")
	}
	// Payload: FullBox version/flags(4) + entry_count(4), then each sample
	// entry begins with its own size(4) and type(4). The codec 4CC (the entry
	// type) therefore sits at payload offset 12 (4+4+4).
	codecOff := int64(12)
	codec := FourCC(buf, int(codecOff))
	if codec == "" {
		return media.New(media.PhaseIndex, media.KindUnsupported, "", stsd.Payload+codecOff,
			"cannot read sample entry codec 4CC")
	}
	switch codec {
	case "avc1", "avc3", "hvc1", "hev1", "mp4a":
	default:
		return media.New(media.PhaseIndex, media.KindUnsupported, "", stsd.Payload+codecOff,
			"unsupported codec 4CC: "+codec)
	}
	tr.Codec = codec
	return nil
}
