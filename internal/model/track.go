package model

// TrackKind enumerates the archivable Matroska track kinds.
type TrackKind string

const (
	KindVideo    TrackKind = "video"
	KindAudio    TrackKind = "audio"
	KindSubtitle TrackKind = "subtitle"
)

// AudioMeta is the raw audio declaration captured verbatim from Tracks.
type AudioMeta struct {
	Channels             uint64  `json:"channels"`
	SamplingFrequency    float64 `json:"sampling_frequency_hz"`
	BitDepth             uint64  `json:"bit_depth,omitempty"`
	ChannelPositions     []byte  `json:"-"`
	HasChannelPositions  bool    `json:"has_channel_layout"`
	PositionsDeclaredRaw string  `json:"positions_declared_raw"`
}

// VideoMeta is the raw video declaration captured from Tracks.
type VideoMeta struct {
	PixelWidth              uint64   `json:"pixel_width"`
	PixelHeight             uint64   `json:"pixel_height"`
	ColorPresent            bool     `json:"color_present"`
	MatrixCoefficients      uint64   `json:"matrix_coefficients,omitempty"`
	ColorRange              uint64   `json:"color_range,omitempty"`
	ColorPrimaries          uint64   `json:"color_primaries,omitempty"`
	TransferCharacteristics uint64   `json:"transfer_characteristics,omitempty"`
	MissingColorFields      []string `json:"missing_color_fields,omitempty"`
}

// Track is one source track's parsed declaration with its source offset so
// validation failures can be anchored to the originating TrackEntry.
type Track struct {
	Number          uint64     `json:"number"`
	UID             uint64     `json:"uid"`
	Kind            TrackKind  `json:"kind"`
	CodecID         string     `json:"codec_id"`
	DefaultDuration uint64     `json:"default_duration_ns,omitempty"`
	JoinUID         int64      `json:"join_uid,omitempty"` // -1 when absent
	HeaderOffset    int64      `json:"-"`
	Audio           *AudioMeta `json:"audio,omitempty"`
	Video           *VideoMeta `json:"video,omitempty"`
}

// Matroska TrackType code → TrackKind mapping helper.
func KindFromTrackType(code uint64) (TrackKind, bool) {
	switch TrackType(code) {
	case TrackTypeVideo:
		return KindVideo, true
	case TrackTypeAudio:
		return KindAudio, true
	case TrackTypeSubtitle:
		return KindSubtitle, true
	default:
		return "", false
	}
}
