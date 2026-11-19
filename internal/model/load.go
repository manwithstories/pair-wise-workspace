package model

import (
	"encoding/json"
	"fmt"
	"os"
)

// LoadTimeline reads and parses a timeline document.
func LoadTimeline(path string) (*Timeline, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseTimeline(b)
}

// ParseTimeline parses a timeline document and validates its structure.
func ParseTimeline(b []byte) (*Timeline, error) {
	var tl Timeline
	if err := json.Unmarshal(b, &tl); err != nil {
		return nil, fmt.Errorf("timeline: %w", err)
	}
	if errs := tl.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("timeline: %v", errs[0])
	}
	return &tl, nil
}

// TrackByID returns a pointer to the named track.
func (tl *Timeline) TrackByID(id string) *Track {
	for i := range tl.Tracks {
		if tl.Tracks[i].ID == id {
			return &tl.Tracks[i]
		}
	}
	return nil
}
