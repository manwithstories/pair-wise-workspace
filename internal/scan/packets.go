package scan

import (
	"encoding/binary"
	"fmt"

	"github.com/asticode/go-astiav"

	"mediacore/internal/clock"
	"mediacore/internal/meta"
)

// scanPackets walks every packet once, recording keyframes and timed metadata.
func scanPackets(fc *astiav.FormatContext, s *Scan, opt Options) error {
	byIndex := make(map[int]Track, len(s.Tracks))
	for _, t := range s.Tracks {
		byIndex[t.Index] = t
	}
	pkt := astiav.AllocPacket()
	if pkt == nil {
		return fmt.Errorf("scan: allocating packet failed")
	}
	defer pkt.Free()
	for {
		if err := fc.ReadFrame(pkt); err != nil {
			if isEOF(err) {
				break
			}
			return fmt.Errorf("scan: read frame: %w", err)
		}
		s.Packets++
		t, ok := byIndex[pkt.StreamIndex()]
		if ok {
			if err := handlePacket(pkt, t, s, opt); err != nil {
				s.Errors = append(s.Errors, err)
			}
		}
		pkt.Unref()
	}
	return nil
}

func handlePacket(pkt *astiav.Packet, t Track, s *Scan, opt Options) error {
	pts := pkt.Pts()
	if pts == astiav.NoPtsValue {
		pts = pkt.Dts()
	}
	var timeUS int64 = clock.NoTimestamp
	var convErr error
	if pts != astiav.NoPtsValue {
		if s.Timeline != nil {
			timeUS, convErr = s.Timeline.ToUnified(t.Index, pts)
		} else {
			timeUS, convErr = clock.Convert(pts, t.TimeBase, clock.Microsecond)
		}
	}
	switch t.Kind {
	case TrackVideo:
		if pkt.Flags().Has(astiav.PacketFlagKey) && len(s.Keyframes) < opt.MaxRecords {
			s.Keyframes = append(s.Keyframes, Keyframe{
				Track: t.Index, PTS: pts, TimeUS: timeUS, BytePos: pkt.Pos(), Size: pkt.Size(),
			})
		}
		// H.264/H.265 SEI travels inside access units; extract it without a
		// decoder so indexing does not depend on frame decoding.
		if t.Codec == "h264" || t.Codec == "hevc" {
			for _, rec := range extractSEI(pkt.Data(), t, timeUS) {
				appendMeta(s, rec, opt)
			}
		}
	case TrackSubtitle:
		rec, err := subtitleRecord(pkt, t, timeUS)
		if err != nil {
			appendMeta(s, MetaRecord{
				Kind: meta.KindSubtitle, Track: t.Index, TimeUS: timeUS, PTS: pts,
				TimeBase: t.TimeBase, Text: preview(pkt.Data()), Quarantined: true, Reason: err.Error(),
			}, opt)
			return err
		}
		if len(rec.Payload) > 0 {
			appendMeta(s, rec, opt)
		}
	case TrackData:
		for _, rec := range dataRecords(pkt, t, timeUS) {
			appendMeta(s, rec, opt)
		}
	}
	if convErr != nil {
		return withTimelineLocation(convErr, t.Index, pts, t.TimeBase)
	}
	return nil
}

func withTimelineLocation(err error, track int, pos int64, tb clock.Rational) error {
	if e, ok := clock.AsError(err); ok {
		return e.WithPosition(track, pos, tb)
	}
	return &clock.Error{Code: clock.CodeOverflow, Op: "scan.packet", Track: track, Position: pos, TimeBase: tb, Err: err}
}

func appendMeta(s *Scan, rec MetaRecord, opt Options) {
	if len(s.Meta) >= opt.MaxRecords {
		return
	}
	s.Meta = append(s.Meta, rec)
}

// preview renders at most 64 printable bytes of a payload for diagnostics.
func preview(b []byte) string {
	const max = 64
	if len(b) > max {
		b = b[:max]
	}
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			out = append(out, c)
		} else {
			out = append(out, '.')
		}
	}
	return string(out)
}

// subtitleRecord parses a subtitle packet into a metadata record.
func subtitleRecord(pkt *astiav.Packet, t Track, timeUS int64) (MetaRecord, error) {
	codec := t.Codec
	if codec == "unknown" || codec == "" {
		codec = "subrip"
	}
	text, tail, err := meta.ParseSubtitle(codec, pkt.Data())
	if err != nil {
		return MetaRecord{}, err
	}
	if text == "" {
		// Empty subtitle samples are padding, not cues; dropping them keeps
		// the metadata index limited to real timed records.
		return MetaRecord{Kind: meta.KindSubtitle, Track: t.Index, Text: "", Payload: nil}, nil
	}
	payload, err := meta.EncodeSubtitle(codec, text, tail)
	if err != nil {
		return MetaRecord{}, err
	}
	return MetaRecord{
		Kind: meta.KindSubtitle, Track: t.Index, TimeUS: timeUS, PTS: pkt.Pts(),
		Duration: pkt.Duration(), TimeBase: t.TimeBase,
		Payload: payload, Text: text,
	}, nil
}

// dataRecords interprets an ID3 (timed_id3) or bin_data payload. Supported
// framings: an emsg box, a 4-byte big-endian length prefix followed by an ID3
// tag (MPEG-TS timed ID3), or a bare ID3 tag.
func dataRecords(pkt *astiav.Packet, t Track, timeUS int64) []MetaRecord {
	return dataRecordsBytes(pkt.Data(), t, timeUS, pkt.Pts())
}

func dataRecordsBytes(data []byte, t Track, timeUS, pts int64) []MetaRecord {
	if len(data) > meta.MaxRecordBytes {
		return []MetaRecord{{
			Kind: meta.KindID3, Track: t.Index, TimeUS: timeUS, PTS: pts,
			TimeBase: t.TimeBase, Quarantined: true,
			Reason: fmt.Sprintf("%v: payload %d bytes", meta.ErrOversize, len(data)),
		}}
	}
	if len(data) >= 8 && string(data[4:8]) == "emsg" {
		return emsgRecords(data, t, timeUS, pts)
	}
	// MPEG-TS timed ID3: 4-byte length prefix then the tag.
	if len(data) >= 14 && binary.BigEndian.Uint32(data[:4]) == uint32(len(data)-4) && string(data[4:7]) == "ID3" {
		data = data[4:]
	}
	tag, err := meta.ParseID3(data)
	if err != nil {
		return []MetaRecord{{
			Kind: meta.KindID3, Track: t.Index, TimeUS: timeUS, PTS: pts,
			TimeBase: t.TimeBase, Quarantined: true, Reason: err.Error(),
		}}
	}
	enc, err := tag.Encode()
	if err != nil {
		return []MetaRecord{{
			Kind: meta.KindID3, Track: t.Index, TimeUS: timeUS, PTS: pts,
			TimeBase: t.TimeBase, Quarantined: true, Reason: err.Error(),
		}}
	}
	return []MetaRecord{{
		Kind: meta.KindID3, Track: t.Index, TimeUS: timeUS, PTS: pts,
		TimeBase: t.TimeBase, Payload: enc, Text: firstText(tag),
	}}
}

func emsgRecords(data []byte, t Track, timeUS, pts int64) []MetaRecord {
	body := data[8:]
	if n := int(binary.BigEndian.Uint32(data[0:4])); n >= 8 && n-8 <= len(body) {
		body = body[:n-8]
	}
	e, err := meta.ParseEmsg(body)
	if err != nil {
		return []MetaRecord{{
			Kind: meta.KindEmsg, Track: t.Index, TimeUS: timeUS, PTS: pts,
			TimeBase: t.TimeBase, Quarantined: true, Reason: err.Error(),
		}}
	}
	enc, err := e.Encode()
	if err != nil {
		return []MetaRecord{{
			Kind: meta.KindEmsg, Track: t.Index, TimeUS: timeUS, PTS: pts,
			TimeBase: t.TimeBase, Quarantined: true, Reason: err.Error(),
		}}
	}
	out := []MetaRecord{{
		Kind: meta.KindEmsg, Track: t.Index, TimeUS: timeUS, PTS: pts,
		Duration: int64(e.PresentationTicks()), TimeBase: t.TimeBase,
		Payload: enc, Text: e.SchemeIDURI,
	}}
	if e.ID3 != nil {
		out = append(out, MetaRecord{
			Kind: meta.KindID3, Track: t.Index, TimeUS: timeUS, PTS: pts,
			TimeBase: t.TimeBase, Payload: e.Message, Text: firstText(e.ID3),
		})
	}
	return out
}

func firstText(tag *meta.ID3Tag) string {
	if tag == nil {
		return ""
	}
	if v, ok := tag.TextFrames()["TIT2"]; ok {
		return v
	}
	for _, v := range tag.TextFrames() {
		return v
	}
	return ""
}

// extractSEI pulls SEI NAL units out of an Annex B or AVCC packet.
func extractSEI(data []byte, t Track, timeUS int64) []MetaRecord {
	if len(data) < 5 {
		return nil
	}
	var nals [][]byte
	if t.Codec == "h264" || t.Codec == "hevc" {
		nals = splitAVCC(data)
		if len(nals) == 0 {
			nals = splitAnnexB(data)
		}
	}
	var out []MetaRecord
	for _, nal := range nals {
		if len(nal) < 2 {
			continue
		}
		typ := int(nal[0] & 0x1f)
		if t.Codec == "hevc" {
			typ = int(nal[0]>>1) & 0x3f
		}
		if typ != 6 {
			continue
		}
		s, err := meta.ParseSEINAL(nal)
		if err != nil {
			out = append(out, MetaRecord{
				Kind: meta.KindSEI, Track: t.Index, TimeUS: timeUS,
				TimeBase: t.TimeBase, Quarantined: true, Reason: err.Error(),
			})
			continue
		}
		if len(s.Messages) == 0 {
			continue
		}
		enc, err := s.Encode(true)
		if err != nil {
			out = append(out, MetaRecord{
				Kind: meta.KindSEI, Track: t.Index, TimeUS: timeUS,
				TimeBase: t.TimeBase, Quarantined: true, Reason: err.Error(),
			})
			continue
		}
		out = append(out, MetaRecord{
			Kind: meta.KindSEI, Track: t.Index, TimeUS: timeUS,
			TimeBase: t.TimeBase, Payload: enc, Text: seiText(s),
		})
	}
	return out
}

func seiText(s *meta.SEI) string {
	for _, m := range s.Messages {
		if m.Type == 5 && len(m.Payload) > 16 {
			return string(m.Payload[16:])
		}
	}
	return ""
}

// splitAVCC splits length-prefixed NAL units (4-byte lengths).
func splitAVCC(b []byte) [][]byte {
	var out [][]byte
	for len(b) >= 4 {
		n := int(binary.BigEndian.Uint32(b[:4]))
		if n <= 0 || n > len(b)-4 {
			return nil
		}
		out = append(out, b[4:4+n])
		b = b[4+n:]
	}
	if len(b) != 0 {
		return nil
	}
	return out
}

// splitAnnexB splits start-code delimited NAL units.
func splitAnnexB(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+3 <= len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				out = append(out, trimZero(b[start:i]))
			}
			start = i + 3
			i += 2
		}
	}
	if start >= 0 && start < len(b) {
		out = append(out, trimZero(b[start:]))
	}
	return out
}

func trimZero(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}
