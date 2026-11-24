// Package testkit generates every fixture the pipeline needs. Nothing here
// reads external files, so a cold checkout can run the full demo and the
// corruption scenarios.
package testkit

import (
	"fmt"

	"mediarun/internal/clock"
	"mediarun/internal/model"
	"mediarun/internal/parse"
)

// Limits keep generated fixtures small and predictable.
const (
	MaxVideoSamples = 220
	MaxAudioSamples = 120
	MaxPackets      = 4000
)

// Scenario selects the transport defect pattern.
type Scenario string

const (
	ScenarioClean     Scenario = "clean"
	ScenarioReorder   Scenario = "reorder"
	ScenarioDuplicate Scenario = "duplicate"
	ScenarioBurst     Scenario = "burst"
	ScenarioWrap      Scenario = "wrap"
)

// ParseScenario validates a --packet-loss style value.
func ParseScenario(s string) (Scenario, error) {
	switch Scenario(s) {
	case ScenarioClean, ScenarioReorder, ScenarioDuplicate, ScenarioBurst, ScenarioWrap:
		return Scenario(s), nil
	default:
		return "", fmt.Errorf("%w: packet-loss %q (want clean|reorder|duplicate|burst|wrap)", model.ErrInvalid, s)
	}
}

// TrackSpec describes one capture source.
type TrackSpec struct {
	ID        uint32
	Handler   string
	Name      string
	Timescale uint32
	Samples   int
	StartTick clock.Tick
}

// Media is a generated movie plus the metadata that must survive a
// round trip.
type Media struct {
	Movie           *parse.Movie
	Bytes           []byte
	SubtitleTrackID uint32
	AudioTrackID    uint32
	VideoTrackID    uint32
	Chapters        []parse.Chapter
	Emsg            []parse.EmsgEvent
	ExpectedEvents  []model.Event
}
