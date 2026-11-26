package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"avpipe/internal/align"
	"avpipe/internal/checkpoint"
	"avpipe/internal/fixture"
	"avpipe/internal/index"
	"avpipe/internal/media"
	"avpipe/internal/ts"
)

// Options are the CLI-independent pipeline inputs.
type Options struct {
	Out            string
	Seed           int
	Case           string
	Bitrates       []int64
	Segments       int
	InterruptAfter int
}

// Analysis is the parsed/indexed state for one rendition.
type Analysis struct {
	Source   fixture.Source
	Packets  []ts.Packet
	Stats    ts.Stats
	PSI      *ts.PSIState
	Clock    *media.Clock
	Index    *index.Index
	VideoPID uint16
	AudioPID uint16
}

// Report contains all terminal and persisted acceptance metrics.
type Report struct {
	Case             string                          `json:"case"`
	Out              string                          `json:"out"`
	ParserVersion    string                          `json:"parser_version"`
	SourceHash       string                          `json:"source_hash"`
	ConfigHash       string                          `json:"config_hash"`
	Packets          int                             `json:"packets"`
	IncompleteTail   int                             `json:"incomplete_tail"`
	FirstDesync      int                             `json:"first_desync_offset"`
	CCGap            int                             `json:"cc_gap"`
	PSICRCReject     int                             `json:"psi_crc_reject"`
	KeyframeFound    int                             `json:"keyframe_found"`
	KeyframeExpected int                             `json:"keyframe_expected"`
	AlignedSlots     int                             `json:"aligned_slots"`
	TotalSlots       int                             `json:"total_slots"`
	GapSlots         int                             `json:"gap_slots"`
	SegmentHashOK    int                             `json:"segment_hash_ok"`
	SegmentHashTotal int                             `json:"segment_hash_total"`
	RoundTripChecked int                             `json:"roundtrip_checked"`
	RoundTripMatched int                             `json:"roundtrip_matched"`
	CheckpointReused int                             `json:"checkpoint_reused,omitempty"`
	Rebuilt          int                             `json:"rebuilt,omitempty"`
	Idempotent       int                             `json:"idempotent,omitempty"`
	OrphansRemoved   []string                        `json:"orphans_removed,omitempty"`
	ClockEpochs      map[string][]media.Epoch        `json:"clock_epochs"`
	Ladder           *align.Ladder                   `json:"ladder,omitempty"`
	Analyses         []Analysis                      `json:"-"`
	Chunks           map[int]*checkpoint.ChunkRecord `json:"chunks,omitempty"`
}

func (r Report) RoundTripPercent() float64 {
	if r.RoundTripChecked == 0 {
		return 0
	}
	return 100 * float64(r.RoundTripMatched) / float64(r.RoundTripChecked)
}

// ConfigHash canonicalizes all generation and processing parameters.
func ConfigHash(opt Options) (string, error) {
	cfg := struct {
		ParserVersion string  `json:"parser_version"`
		Seed          int     `json:"seed"`
		Case          string  `json:"case"`
		Bitrates      []int64 `json:"bitrates"`
		Segments      int     `json:"segments"`
		ChunkPackets  int     `json:"chunk_packets"`
	}{
		ParserVersion: media.ParserVersion,
		Seed:          opt.Seed,
		Case:          opt.Case,
		Bitrates:      append([]int64(nil), opt.Bitrates...),
		Segments:      opt.Segments,
		ChunkPackets:  fixture.PacketsPerSlot,
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func toFixtureConfig(opt Options) fixture.Config {
	return fixture.Config{
		Seed:     opt.Seed,
		Case:     fixture.Case(opt.Case),
		Bitrates: append([]int64(nil), opt.Bitrates...),
		Segments: opt.Segments,
	}
}
