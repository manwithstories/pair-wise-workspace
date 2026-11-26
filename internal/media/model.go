package media

// TrackKind identifies the representation carried by a track.
type TrackKind string

const (
	KindVideo TrackKind = "vide"
	KindAudio TrackKind = "soun"
	KindData  TrackKind = "meta"
)

// Sample is the normalized intersection of stts/ctts/stsz/stsc/stco/stss.
type Sample struct {
	Index    uint32 `json:"index"` // one-based MP4 sample number
	DTS      int64  `json:"dts"`
	PTS      int64  `json:"pts"`
	Duration uint32 `json:"duration"`
	Size     uint32 `json:"size"`
	Offset   int64  `json:"offset"`
	Sync     bool   `json:"sync"`

	// Simulation/recovery bookkeeping. It is not media payload and does not
	// affect the deterministic repaired MP4 bytes.
	Lost            bool `json:"lost,omitempty"`
	Recovered       bool `json:"recovered,omitempty"`
	Retransmissions int  `json:"retransmissions,omitempty"`
	Unrecovered     bool `json:"unrecovered,omitempty"`
}

// EditEntry is a version-independent edit list entry.
type EditEntry struct {
	SegmentDuration uint64 `json:"segment_duration"`
	MediaTime       int64  `json:"media_time"`
	MediaRate       int16  `json:"media_rate"`
}

// AudioMetadata records both declared and validated audio properties.
type AudioMetadata struct {
	SampleRate       uint32   `json:"sample_rate"`
	ChannelCount     uint16   `json:"channel_count"`
	Layout           []string `json:"layout"`
	DeclaredRate     uint32   `json:"declared_rate,omitempty"`
	DeclaredChannels uint16   `json:"declared_channels,omitempty"`
	Source           string   `json:"source,omitempty"`
	RepairBasis      string   `json:"repair_basis,omitempty"`
}

// ColorMetadata preserves original color declarations and normalization result.
type ColorMetadata struct {
	Primaries      string `json:"primaries"`
	Transfer       string `json:"transfer"`
	Matrix         string `json:"matrix"`
	FullRange      bool   `json:"full_range"`
	BitDepth       uint8  `json:"bit_depth"`
	Original       string `json:"original,omitempty"`
	ConflictDetail string `json:"conflict_detail,omitempty"`
	RepairBasis    string `json:"repair_basis,omitempty"`
}

// Track is a normalized multitrack timeline.
type Track struct {
	ID          uint32         `json:"id"`
	Kind        TrackKind      `json:"kind"`
	Timescale   uint32         `json:"timescale"`
	Width       uint16         `json:"width,omitempty"`
	Height      uint16         `json:"height,omitempty"`
	BitrateKbps int            `json:"bitrate_kbps,omitempty"`
	Samples     []Sample       `json:"samples"`
	Edits       []EditEntry    `json:"edits,omitempty"`
	Audio       *AudioMetadata `json:"audio,omitempty"`
	Color       *ColorMetadata `json:"color,omitempty"`
	Faults      []*Fault       `json:"faults,omitempty"`
}

// Project is the deterministic in-memory media processing document.
type Project struct {
	MovieTimescale uint32   `json:"movie_timescale"`
	DurationTicks  uint64   `json:"duration_ticks"`
	Tracks         []*Track `json:"tracks"`
	Faults         []*Fault `json:"faults,omitempty"`
	SourceFormat   string   `json:"source_format,omitempty"`
}

// Clock returns the track's rational media clock.
func (t *Track) Clock() (Clock, error) { return NewClock(t.Timescale) }

// SyncSampleNumbers returns one-based sync sample numbers.
func (t *Track) SyncSampleNumbers() []uint32 {
	out := make([]uint32, 0)
	for _, s := range t.Samples {
		if s.Sync {
			out = append(out, s.Index)
		}
	}
	return out
}

// FrameDurationNS returns the common duration between consecutive DTS values.
func (t *Track) FrameDurationNS() (int64, error) {
	if len(t.Samples) < 2 {
		return 0, nil
	}
	clock, err := t.Clock()
	if err != nil {
		return 0, err
	}
	return AbsDiffNS(t.Samples[0].DTS, t.Samples[1].DTS, clock)
}
