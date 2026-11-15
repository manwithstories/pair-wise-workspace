package playlist

// MediaEntry is one row of a media playlist: either a real segment or a GAP
// placeholder standing in for a missing sequence number.
type MediaEntry struct {
	URI        string  `json:"uri"`
	DurationS  float64 `json:"duration_s"`
	IsGap      bool    `json:"is_gap"`
	Seq        int64   `json:"seq"`
	SourceTSMS int64   `json:"source_ts_ms"`
	MeasuredMS int64   `json:"measured_ms"`
	DeclaredMS int64   `json:"declared_ms"`
}

// MediaPlaylist is the in-memory representation of one rendition playlist.
type MediaPlaylist struct {
	Rendition      int          `json:"rendition"`
	Name           string       `json:"name"`
	BandwidthBps   int          `json:"bandwidth_bps"`
	Resolution     string       `json:"resolution"`
	TargetDuration int          `json:"target_duration"`
	MediaSequence  int64        `json:"media_sequence"`
	Entries        []MediaEntry `json:"entries"`
	FileName       string       `json:"file_name"`
}

// MediaRef is one EXT-X-STREAM-INF row of the master playlist.
type MediaRef struct {
	BandwidthBps int    `json:"bandwidth_bps"`
	Resolution   string `json:"resolution"`
	Name         string `json:"name"`
	URI          string `json:"uri"`
}

// MasterPlaylist references the four rendition playlists.
type MasterPlaylist struct {
	Version        int        `json:"version"`
	TargetDuration int        `json:"target_duration"`
	Media          []MediaRef `json:"media"`
	FileName       string     `json:"file_name"`
}
