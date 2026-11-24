package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"mediarun/internal/model"
	"mediarun/internal/txn"
)

func checkReload(dir string) error {
	m, err := txn.LoadManifest(dir)
	if err != nil {
		return err
	}
	for _, s := range m.Segments {
		data, hash, err := txn.ReadSegment(dir, s.Name)
		if err != nil {
			return err
		}
		if hash != s.SHA256 {
			return fmt.Errorf("%w: segment %q hash drift", model.ErrCommit, s.Name)
		}
		var payload txn.SegmentPayload
		if err := json.Unmarshal(data, &payload); err != nil {
			return fmt.Errorf("%w: segment %q decode: %v", model.ErrCorruptIndex, s.Name, err)
		}
		if payload.Name != s.Name || len(payload.Events) != s.Events {
			return fmt.Errorf("%w: segment %q metadata mismatch", model.ErrCorruptIndex, s.Name)
		}
	}
	return nil
}

// summarizeGaps renders the transport gaps as a stable, human-readable
// scope string for the verification summary.
func summarizeGaps(gaps []model.Gap) string {
	if len(gaps) == 0 {
		return "none"
	}
	sort.SliceStable(gaps, func(i, j int) bool {
		if gaps[i].TrackID != gaps[j].TrackID {
			return gaps[i].TrackID < gaps[j].TrackID
		}
		return gaps[i].From < gaps[j].From
	})
	var parts []string
	last := gaps[0].TrackID
	packets := 0
	for _, g := range gaps {
		if g.TrackID != last {
			parts = append(parts, fmt.Sprintf("track-%d:%d", last, packets))
			last = g.TrackID
			packets = 0
		}
		packets += g.Count
	}
	parts = append(parts, fmt.Sprintf("track-%d:%d", last, packets))
	return fmt.Sprintf("gaps=%d packets=%s", len(gaps), strings.Join(parts, ","))
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
