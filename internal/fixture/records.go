package fixture

import (
	"encoding/json"
	"os"

	"archivectl/internal/clock"
	"archivectl/internal/core"
	"archivectl/internal/verify"
)

// ClockFile is the generator-produced device clock sidecar.
type ClockFile struct {
	StartTick     uint32              `json:"start_tick"`
	TicksPerFrame core.Rational       `json:"ticks_per_frame"`
	WrapInjected  bool                `json:"wrap_injected"`
	DriftInjected bool                `json:"drift_injected"`
	Blocks        []clock.BlockRecord `json:"blocks"`
	Anchors       []clock.Anchor      `json:"anchors"`
}

// ChecksFile contains variable-block CRC records.
type ChecksFile struct {
	SegmentFrames int                 `json:"segment_frames"`
	Blocks        []verify.BlockCheck `json:"blocks"`
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

// LoadClockFile reads a clock sidecar.
func LoadClockFile(path string) (*ClockFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, core.Wrap(core.KindIO, "fixture.LoadClockFile", path, err)
	}
	var v ClockFile
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, core.Wrap(core.KindIndexCorrupt, "fixture.LoadClockFile", path, err)
	}
	return &v, nil
}

// LoadChecksFile reads block CRC records.
func LoadChecksFile(path string) (*ChecksFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, core.Wrap(core.KindIO, "fixture.LoadChecksFile", path, err)
	}
	var v ChecksFile
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, core.Wrap(core.KindIndexCorrupt, "fixture.LoadChecksFile", path, err)
	}
	return &v, nil
}
