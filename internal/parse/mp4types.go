package parse

// Limits used by track parsing.
const (
	MaxSamples     = 4096
	MaxSampleBytes = 8 << 20
)

// Raw tracking types for the parsed movie.

// Sample is one media sample as described by the sample tables.
type Sample struct {
	Index    int    `json:"index"` // 1-based, as used by stss
	Offset   int64  `json:"offset"`
	Size     uint32 `json:"size"`
	Duration uint32 `json:"duration"` // in track timescale units
	DTS      uint64 `json:"dts"`      // in track timescale units
	Sync     bool   `json:"sync"`
	Data     []byte `json:"-"`
}

// Track is a parsed media track.
type Track struct {
	ID        uint32   `json:"id"`
	Handler   string   `json:"handler"` // vide, soun, text, meta
	Timescale uint32   `json:"timescale"`
	Duration  uint64   `json:"duration"`
	Width     uint16   `json:"width,omitempty"`
	Height    uint16   `json:"height,omitempty"`
	Language  string   `json:"language,omitempty"`
	Entry     RawBox   `json:"-"` // stsd sample entry, preserved verbatim
	Samples   []Sample `json:"samples"`
	Keyframes []uint32 `json:"keyframes"` // 1-based sample numbers
}

// Movie is a parsed ISO BMFF file. Unknown boxes are retained raw so a
// rewrite keeps them byte-for-byte.
type Movie struct {
	Timescale uint32   `json:"timescale"`
	Duration  uint64   `json:"duration"`
	Tracks    []Track  `json:"tracks"`
	Extras    []RawBox `json:"-"` // unknown top-level boxes, in order
	Udta      []RawBox `json:"-"` // unknown children of moov/udta
	HasMoov   bool     `json:"-"`
	HasMdat   bool     `json:"-"`
}
