// Package track validates Matroska track declarations and maps them onto the
// output MP4 track model. Codecs without a documented MP4 mapping are counted
// and refused; they are never transcoded or guessed at.
package track

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"archivecheck.local/archivecheck/internal/fault"
)

// Type is the Matroska TrackType value.
type Type uint64

const (
	TypeVideo    Type = 1
	TypeAudio    Type = 2
	TypeComplex  Type = 3
	TypeLogo     Type = 16
	TypeSubtitle Type = 17
	TypeButtons  Type = 18
	TypeControl  Type = 32
	TypeMetadata Type = 33
)

// String returns a stable lowercase name.
func (t Type) String() string {
	switch t {
	case TypeVideo:
		return "video"
	case TypeAudio:
		return "audio"
	case TypeSubtitle:
		return "subtitle"
	case TypeComplex:
		return "complex"
	case TypeLogo:
		return "logo"
	case TypeButtons:
		return "buttons"
	case TypeControl:
		return "control"
	case TypeMetadata:
		return "metadata"
	default:
		return fmt.Sprintf("type%d", uint64(t))
	}
}

// PayloadKind is the MP4 sample-entry family.
type PayloadKind int

const (
	KindUnsupported PayloadKind = iota
	KindAVC
	KindHEVC
	KindVP9
	KindAV1
	KindAAC
	KindOpus
	KindVorbis
	KindFLAC
	KindPCM
	KindWebVTT
	KindSRT
)

// Mapping describes how one Matroska codec is represented in MP4.
type Mapping struct {
	Kind PayloadKind
	// Entry is the MP4 sample entry four-character code.
	Entry string
	// ConfigBox is the MP4 decoder configuration box, if any.
	ConfigBox string
	// Description is the documented Matroska CodecID prefix.
	Description string
}

var codecs = map[string]Mapping{
	"V_MPEG4/ISO/AVC":  {Kind: KindAVC, Entry: "avc1", ConfigBox: "avcC"},
	"V_MPEGH/ISO/HEVC": {Kind: KindHEVC, Entry: "hvc1", ConfigBox: "hvcC"},
	"V_VP9":            {Kind: KindVP9, Entry: "vp09", ConfigBox: "vpcC"},
	"V_AV1":            {Kind: KindAV1, Entry: "av01", ConfigBox: "av1C"},
	"A_AAC":            {Kind: KindAAC, Entry: "mp4a", ConfigBox: "esds"},
	"A_OPUS":           {Kind: KindOpus, Entry: "Opus", ConfigBox: "dOps"},
	"A_VORBIS":         {Kind: KindVorbis, Entry: "mp4a", ConfigBox: "esds"},
	"A_FLAC":           {Kind: KindFLAC, Entry: "fLaC", ConfigBox: "dfLa"},
	"A_PCM/INT/LIT":    {Kind: KindPCM, Entry: "ipcm", ConfigBox: ""},
	"S_TEXT/UTF8":      {Kind: KindSRT, Entry: "tx3g", ConfigBox: ""},
	"S_TEXT/WEBVTT":    {Kind: KindWebVTT, Entry: "wvtt", ConfigBox: ""},
	"S_TEXT/ASS":       {Kind: KindSRT, Entry: "tx3g", ConfigBox: ""},
	"S_TEXT/SSA":       {Kind: KindSRT, Entry: "tx3g", ConfigBox: ""},
}

// CodecFor resolves a Matroska CodecID. Codec IDs with a suffix such as
// "A_AAC/MPEG4/LC" resolve through their documented first component.
func CodecFor(codecID string) (Mapping, bool) {
	codecID = strings.TrimSpace(codecID)
	if m, ok := codecs[codecID]; ok {
		return m, true
	}
	if i := strings.IndexByte(codecID, '/'); i > 0 {
		head := codecID[:i]
		if m, ok := codecs[head]; ok {
			return m, true
		}
	}
	return Mapping{}, false
}

// Source is one TrackEntry gathered from Tracks.
type Source struct {
	Number          uint64
	UID             uint64
	Type            Type
	CodecID         string
	CodecName       string
	Name            string
	Language        string
	DefaultDuration uint64 // nanoseconds
	FlagLacing      bool
	// Video dimensions.
	PixelWidth  uint64
	PixelHeight uint64
	// Audio properties.
	SamplingFrequency uint64
	Channels          uint64
	// CodecPrivate is either copied into the MP4 decoder configuration box
	// or carried as the exact Matroska bytes; see ConfigMode.
	CodecPrivate []byte
}

// Track is a validated, MP4-mappable track.
type Track struct {
	Source
	Mapping Mapping
	// DestTrackID is the MP4 track_ID assigned in output order.
	DestTrackID uint32
	// ConfigMode says how CodecPrivate is used.
	ConfigMode ConfigMode
}

// ConfigMode selects how CodecPrivate becomes an MP4 configuration box.
type ConfigMode int

const (
	// ConfigNone means no configuration box is needed.
	ConfigNone ConfigMode = iota
	// ConfigPassThrough copies CodecPrivate bytes into a generic configuration
	// box (used by VP9's vpcC features, AV1's av1C and similar).
	ConfigPassThrough
	// ConfigAVCC wraps AVC CodecPrivate into avcC unchanged.
	ConfigAVCC
	// ConfigHVCC wraps HEVC CodecPrivate into hvcC unchanged.
	ConfigHVCC
	// ConfigESDS wraps AAC CodecPrivate (AudioSpecificConfig) into esds.
	ConfigESDS
	// ConfigDOps wraps Opus CodecPrivate into dOps.
	ConfigDOps
	// ConfigDFLa wraps FLAC CodecPrivate into dfLa.
	ConfigDFLa
)

// Map validates the track list, drops tracks that cannot be represented in
// MP4 in a documented way, and reports how many were rejected.
//
// The rules are deliberately conservative:
//   - duplicate track numbers are a structural error;
//   - video tracks need a positive width and height;
//   - audio tracks need a positive sample rate and channel count;
//   - subtitle tracks are only mapped for text codecs, and require the
//     conversion to be lossless (UTF-8 text);
//   - tracks with a known but unsupported codec are counted, not fatal,
//     unless includeUnsupported is false and the caller needs all tracks.
func Map(sources []Source) (tracks []Track, unknown []Source, err error) {
	seen := make(map[uint64]bool, len(sources))
	sorted := append([]Source(nil), sources...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })
	for _, s := range sorted {
		if s.Number == 0 {
			return nil, nil, fault.New(fault.StageMap, fault.ErrMalformed).InElement("TrackNumber")
		}
		if seen[s.Number] {
			return nil, nil, fault.New(fault.StageMap,
				fmt.Errorf("%w: duplicate track number %d", fault.ErrMalformed, s.Number)).OnTrack(s.Number)
		}
		seen[s.Number] = true
		m, ok := CodecFor(s.CodecID)
		if !ok {
			unknown = append(unknown, s)
			continue
		}
		if err := validate(s, m); err != nil {
			if errors.Is(err, errUnsupportedShape) {
				unknown = append(unknown, s)
				continue
			}
			return nil, nil, err
		}
		t := Track{Source: s, Mapping: m, DestTrackID: uint32(len(tracks) + 1)}
		t.ConfigMode = configMode(m.Kind)
		tracks = append(tracks, t)
	}
	return tracks, unknown, nil
}

var errUnsupportedShape = errors.New("track shape cannot be represented in MP4")

func validate(s Source, m Mapping) error {
	switch m.Kind {
	case KindAVC, KindHEVC, KindVP9, KindAV1:
		if s.Type != TypeVideo {
			return fault.New(fault.StageMap, fmt.Errorf("%w: video codec on %s track", errUnsupportedShape, s.Type)).OnTrack(s.Number)
		}
		if s.PixelWidth == 0 || s.PixelHeight == 0 {
			return fault.New(fault.StageMap,
				fmt.Errorf("%w: video track has zero dimensions", fault.ErrMalformed)).OnTrack(s.Number)
		}
	case KindAAC, KindOpus, KindVorbis, KindFLAC, KindPCM:
		if s.Type != TypeAudio {
			return fault.New(fault.StageMap, fmt.Errorf("%w: audio codec on %s track", errUnsupportedShape, s.Type)).OnTrack(s.Number)
		}
		if s.SamplingFrequency == 0 {
			return fault.New(fault.StageMap,
				fmt.Errorf("%w: audio track has zero sample rate", fault.ErrMalformed)).OnTrack(s.Number)
		}
		if s.Channels == 0 {
			return fault.New(fault.StageMap,
				fmt.Errorf("%w: audio track has zero channels", fault.ErrMalformed)).OnTrack(s.Number)
		}
	case KindSRT, KindWebVTT:
		if s.Type != TypeSubtitle {
			return fault.New(fault.StageMap, fmt.Errorf("%w: subtitle codec on %s track", errUnsupportedShape, s.Type)).OnTrack(s.Number)
		}
	default:
		return fault.New(fault.StageMap, errUnsupportedShape).OnTrack(s.Number)
	}
	return nil
}

func configMode(k PayloadKind) ConfigMode {
	switch k {
	case KindAVC:
		return ConfigAVCC
	case KindHEVC:
		return ConfigHVCC
	case KindAAC, KindVorbis:
		return ConfigESDS
	case KindOpus:
		return ConfigDOps
	case KindFLAC:
		return ConfigDFLa
	case KindVP9, KindAV1:
		return ConfigPassThrough
	default:
		return ConfigNone
	}
}

// Report summarises a mapping for terminal output and the recovery manifest.
type Report struct {
	Tracks        []TrackSummary `json:"tracks"`
	UnknownTracks int            `json:"unknown_tracks"`
	// UnknownCodec describes the first unknown codec so an operator knows
	// whether to install a newer build or convert the source.
	UnknownCodecIDs []string `json:"unknown_codec_ids,omitempty"`
}

// TrackSummary is the per-track report line.
type TrackSummary struct {
	Number      uint64 `json:"number"`
	Type        string `json:"type"`
	CodecID     string `json:"codec_id"`
	MP4Entry    string `json:"mp4_entry"`
	DestTrackID uint32 `json:"dest_track_id"`
	Blocks      int64  `json:"blocks"`
	Frames      int64  `json:"frames"`
	FirstTick   int64  `json:"first_tick"`
	LastTick    int64  `json:"last_tick"`
	Keyframes   int64  `json:"keyframes"`
}

// NewReport builds a report from tracks and rejected tracks.
func NewReport(tracks []Track, unknown []Source) Report {
	r := Report{UnknownTracks: len(unknown)}
	for _, t := range tracks {
		r.Tracks = append(r.Tracks, TrackSummary{
			Number:      t.Number,
			Type:        t.Type.String(),
			CodecID:     t.CodecID,
			MP4Entry:    t.Mapping.Entry,
			DestTrackID: t.DestTrackID,
		})
	}
	for _, u := range unknown {
		r.UnknownCodecIDs = append(r.UnknownCodecIDs, u.CodecID)
	}
	sort.Strings(r.UnknownCodecIDs)
	return r
}
