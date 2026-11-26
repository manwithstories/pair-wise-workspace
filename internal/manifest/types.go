// Package manifest parses the supplier's self-describing material list.
// Parsing is strict: malformed JSON, unsupported codecs and inconsistent
// timebases produce fatal model errors rather than partially loaded state.
// Every packet record carries its source byte offset and raw PTS, which the
// clock and timeline subsystems rely on for provenance.
package manifest

// Packet kinds.
const (
	KindMedia  = "media"  // elementary stream packet
	KindParity = "parity" // XOR check group parity packet
)

// Stream kinds.
const (
	KindVideo = "video"
	KindAudio = "audio"
)

// SupportedCodecs is the whitelist enforced on every stream.
var SupportedCodecs = map[string]bool{
	"h264": true,
	"aac":  true,
}

// Container is fixed to MP4 for every declared stream.
const ContainerMP4 = "mp4"

// Manifest is the top-level self-describing document.
type Manifest struct {
	Version    string  `json:"version"`
	Seed       uint64  `json:"seed"`
	DurationS  int64   `json:"duration_s"`
	MasterHz   int64   `json:"master_hz"`
	GeneratedAt string `json:"generated_at"` // deterministic label, not wall clock
	Streams    []Stream `json:"streams"`
	Anchors    []Anchor `json:"anchors"`
	Injection  Injection `json:"injection"`
	Packets    []Packet `json:"packets"`
}

// Stream describes one declared media track and its supplier timebase.
type Stream struct {
	ID           int    `json:"id"`
	Kind         string `json:"kind"`
	Codec        string `json:"codec"`
	Container    string `json:"container"`
	File         string `json:"file"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	TimebaseNum  int64  `json:"timebase_num"`
	TimebaseDen  int64  `json:"timebase_den"`
	StartPTS     int64  `json:"start_pts"`       // non-zero source start
	WrapPeriod   int64  `json:"wrap_period"`     // 0 disables wrap expansion
	KeyframeEvery int  `json:"keyframe_every"`  // sparse GOP spacing in packets
	EditList     []EditRange `json:"edit_list"`
	Drift        DriftSpec `json:"drift"`
}

// EditRange is one edit list segment expressed in source ticks.
type EditRange struct {
	StartPTS int64 `json:"start_pts"`
	EndPTS   int64 `json:"end_pts"`
}

// DriftSpec is the piecewise-linear clock drift model for a stream.
// Effective offset at time t (seconds) is offsetS + slope*segmentIndex.
type DriftSpec struct {
	OffsetS float64 `json:"offset_s"`
	SlopeS  float64 `json:"slope_s"` // per-segment drift step
	SegmentS int64  `json:"segment_s"`
}

// Anchor is a supplier sync point between a stream and the master clock.
type Anchor struct {
	StreamID    int   `json:"stream_id"`
	AtSecond    int64 `json:"at_second"`
	SourcePTS   int64 `json:"source_pts"` // raw, possibly pre-wrap value
	MasterTicks int64 `json:"master_ticks"`
	Conflicts   bool  `json:"conflicts,omitempty"` // deliberately bad anchor
}

// Packet is one numbered record in a stream's packet stream.
type Packet struct {
	StreamID     int    `json:"stream_id"`
	Seq          int64  `json:"seq"`
	Kind         string `json:"kind"`
	GroupID      int64  `json:"group_id"`       // XOR check group
	SourceOffset int64  `json:"source_offset"`  // supplier byte offset
	RawPTS       int64  `json:"raw_pts"`        // source timestamp pre-wrap
	Keyframe     bool   `json:"keyframe,omitempty"`
	DurationTick int64  `json:"duration_tick"`  // VFR duration in source ticks
	PayloadLen   int    `json:"payload_len"`
	// XOR is the check byte. On a parity packet it equals the XOR of every
	// media byte of GroupID; on media packets it is that packet's own byte.
	XOR byte `json:"xor"`
}

// Injection records every anomaly the fixture generator introduced. The
// pipeline replays these deterministically instead of sampling at runtime.
type Injection struct {
	LossRate          float64 `json:"loss_rate"`
	WrappedStreams    []int   `json:"wrapped_streams"`
	Duplicates        []Dup   `json:"duplicates"`
	TruncateAfterSeq  []Trunc `json:"truncate_after_seq"`
	CorruptManifest   bool    `json:"corrupt_manifest"`
	AnchorConflicts   int     `json:"anchor_conflicts"`
}

// Dup marks one duplicated (stream,seq) pair.
type Dup struct {
	StreamID int   `json:"stream_id"`
	Seq      int64 `json:"seq"`
}

// Trunc marks where a stream's captured tail was cut.
type Trunc struct {
	StreamID      int   `json:"stream_id"`
	AfterSeq      int64 `json:"after_seq"`
	DeclaredCount int64 `json:"declared_count"`
}

// VideoStreamCount returns how many video tracks are declared.
func (m *Manifest) VideoStreamCount() int {
	n := 0
	for _, s := range m.Streams {
		if s.Kind == KindVideo {
			n++
		}
	}
	return n
}

// StreamByID looks up a stream, returning false when absent.
func (m *Manifest) StreamByID(id int) (Stream, bool) {
	for _, s := range m.Streams {
		if s.ID == id {
			return s, true
		}
	}
	return Stream{}, false
}
