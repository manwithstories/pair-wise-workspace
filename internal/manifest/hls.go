// Package manifest parses HLS and DASH manifests into normalized
// semantic forms and verifies that a parse/serialize/parse round trip
// preserves them. The first differing field is reported on mismatch.
package manifest

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mediascan/internal/core"

	"github.com/Eyevinn/hls-m3u8/m3u8"
)

// MismatchError reports the first semantically differing field found
// during a round-trip comparison.
type MismatchError struct {
	Manifest string
	Field    string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("%s: roundtrip mismatch at %s", e.Manifest, e.Field)
}

// HLSSegment is the normalized semantic view of one media segment.
type HLSSegment struct {
	URI           string
	Duration      core.Rat
	Discontinuity bool
	HasPDT        bool
	PDTUnixMilli  int64
}

// HLSMedia is a normalized media playlist.
type HLSMedia struct {
	TargetDuration uint
	EndList        bool
	Segments       []HLSSegment
}

// HLSVariant is one normalized master-playlist variant.
type HLSVariant struct {
	URI        string
	Bandwidth  uint32
	Codecs     string
	Resolution string
}

// HLSMaster is a normalized master playlist.
type HLSMaster struct {
	Variants []HLSVariant
}

// ratFromFloat converts a seconds value to a Rat with microsecond
// precision, rejecting negative values (illegal manifest durations).
func ratFromFloat(f float64, field string) (core.Rat, error) {
	if f < 0 {
		return core.Rat{}, fmt.Errorf("invalid duration: %v is negative", f)
	}
	s := strconv.FormatFloat(f, 'f', 6, 64)
	parts := strings.SplitN(s, ".", 2)
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return core.Rat{}, fmt.Errorf("invalid duration %q: %v", field, err)
	}
	frac, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return core.Rat{}, fmt.Errorf("invalid duration %q: %v", field, err)
	}
	return core.NewRat(whole*1_000_000+frac, 1_000_000), nil
}

// ParseHLSMedia parses and normalizes an HLS media playlist.
func ParseHLSMedia(data []byte) (*HLSMedia, error) {
	pl, lt, err := m3u8.DecodeFrom(bytes.NewReader(data), false)
	if err != nil {
		return nil, fmt.Errorf("hls media: %w", err)
	}
	if lt != m3u8.MEDIA {
		return nil, fmt.Errorf("hls media: not a media playlist")
	}
	mp := pl.(*m3u8.MediaPlaylist)
	out := &HLSMedia{TargetDuration: mp.TargetDuration, EndList: mp.Closed}
	for _, seg := range mp.Segments {
		if seg == nil {
			continue
		}
		d, err := ratFromFloat(seg.Duration, seg.URI)
		if err != nil {
			return nil, err
		}
		ns := HLSSegment{URI: seg.URI, Duration: d, Discontinuity: seg.Discontinuity}
		if !seg.ProgramDateTime.IsZero() {
			ns.HasPDT = true
			ns.PDTUnixMilli = seg.ProgramDateTime.UnixMilli()
		}
		out.Segments = append(out.Segments, ns)
	}
	return out, nil
}

// ParseHLSMaster parses and normalizes an HLS master playlist.
func ParseHLSMaster(data []byte) (*HLSMaster, error) {
	pl, lt, err := m3u8.DecodeFrom(bytes.NewReader(data), false)
	if err != nil {
		return nil, fmt.Errorf("hls master: %w", err)
	}
	if lt != m3u8.MASTER {
		return nil, fmt.Errorf("hls master: not a master playlist")
	}
	mp := pl.(*m3u8.MasterPlaylist)
	out := &HLSMaster{}
	for _, v := range mp.Variants {
		if v == nil {
			continue
		}
		out.Variants = append(out.Variants, HLSVariant{
			URI:        v.URI,
			Bandwidth:  v.Bandwidth,
			Codecs:     v.Codecs,
			Resolution: v.Resolution,
		})
	}
	return out, nil
}

// EncodeHLSMedia serializes the normalized form back to playlist text.
func EncodeHLSMedia(m *HLSMedia) ([]byte, error) {
	p, err := m3u8.NewMediaPlaylist(0, uint(len(m.Segments)+1))
	if err != nil {
		return nil, err
	}
	p.MediaType = m3u8.VOD
	for _, s := range m.Segments {
		seg := &m3u8.MediaSegment{
			URI:           s.URI,
			Duration:      s.Duration.Float64(),
			Discontinuity: s.Discontinuity,
		}
		if s.HasPDT {
			seg.ProgramDateTime = time.UnixMilli(s.PDTUnixMilli).UTC()
		}
		if err := p.AppendSegment(seg); err != nil {
			return nil, err
		}
	}
	p.SetTargetDuration(m.TargetDuration)
	if m.EndList {
		p.Close()
	}
	return p.Encode().Bytes(), nil
}

// EncodeHLSMaster serializes a normalized master playlist.
func EncodeHLSMaster(m *HLSMaster) ([]byte, error) {
	p := m3u8.NewMasterPlaylist()
	for _, v := range m.Variants {
		p.Append(v.URI, nil, m3u8.VariantParams{
			Bandwidth:  v.Bandwidth,
			Codecs:     v.Codecs,
			Resolution: v.Resolution,
		})
	}
	return p.Encode().Bytes(), nil
}

// diffMedia returns the first differing field, or "" when equal.
func diffMedia(a, b *HLSMedia) string {
	if a.TargetDuration != b.TargetDuration {
		return "target-duration"
	}
	if a.EndList != b.EndList {
		return "endlist"
	}
	if len(a.Segments) != len(b.Segments) {
		return "segments.count"
	}
	for i := range a.Segments {
		x, y := a.Segments[i], b.Segments[i]
		field := fmt.Sprintf("segments[%d]", i)
		switch {
		case x.URI != y.URI:
			return field + ".uri"
		case x.Duration != y.Duration:
			return field + ".duration"
		case x.Discontinuity != y.Discontinuity:
			return field + ".discontinuity"
		case x.HasPDT != y.HasPDT:
			return field + ".program-date-time.present"
		case x.HasPDT && x.PDTUnixMilli != y.PDTUnixMilli:
			return field + ".program-date-time"
		}
	}
	return ""
}

// diffMaster returns the first differing field, or "" when equal.
func diffMaster(a, b *HLSMaster) string {
	if len(a.Variants) != len(b.Variants) {
		return "variants.count"
	}
	for i := range a.Variants {
		x, y := a.Variants[i], b.Variants[i]
		field := fmt.Sprintf("variants[%d]", i)
		switch {
		case x.URI != y.URI:
			return field + ".uri"
		case x.Bandwidth != y.Bandwidth:
			return field + ".bandwidth"
		case x.Codecs != y.Codecs:
			return field + ".codecs"
		case x.Resolution != y.Resolution:
			return field + ".resolution"
		}
	}
	return ""
}

// RoundTripHLSMedia parses, re-encodes and re-parses a media playlist,
// comparing normalized semantics. The first differing field is reported.
func RoundTripHLSMedia(name string, data []byte) (*HLSMedia, error) {
	a, err := ParseHLSMedia(data)
	if err != nil {
		return nil, err
	}
	enc, err := EncodeHLSMedia(a)
	if err != nil {
		return nil, err
	}
	b, err := ParseHLSMedia(enc)
	if err != nil {
		return nil, fmt.Errorf("hls media: re-parse after encode: %w", err)
	}
	if f := diffMedia(a, b); f != "" {
		return nil, &MismatchError{Manifest: name, Field: f}
	}
	return a, nil
}

// RoundTripHLSMaster round-trips a master playlist like RoundTripHLSMedia.
func RoundTripHLSMaster(name string, data []byte) (*HLSMaster, error) {
	a, err := ParseHLSMaster(data)
	if err != nil {
		return nil, err
	}
	enc, err := EncodeHLSMaster(a)
	if err != nil {
		return nil, err
	}
	b, err := ParseHLSMaster(enc)
	if err != nil {
		return nil, fmt.Errorf("hls master: re-parse after encode: %w", err)
	}
	if f := diffMaster(a, b); f != "" {
		return nil, &MismatchError{Manifest: name, Field: f}
	}
	return a, nil
}
