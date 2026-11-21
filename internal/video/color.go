// Package video validates and retains Matroska colour metadata. Missing or
// incomplete colour fields are never invented: strict policy rejects them,
// explicit retain policy carries them into manifest warnings.
package video

import (
	"fmt"

	"mkvarchive/internal/model"
)

// RetentionPolicy mirrors the audio package's explicit-policy model.
type RetentionPolicy int

const (
	PolicyReject RetentionPolicy = iota // refuse incomplete colour metadata
	PolicyRetain                        // retain raw values, warn on gaps
)

// ColorReport captures one video track's colour declaration verbatim.
type ColorReport struct {
	SourceTrackUID uint64   `json:"source_track_uid"`
	ColorPresent   bool     `json:"color_present"`
	Matrix         uint64   `json:"matrix_coefficients,omitempty"`
	Range          uint64   `json:"color_range,omitempty"`
	Primaries      uint64   `json:"color_primaries,omitempty"`
	Transfer       uint64   `json:"transfer_characteristics,omitempty"`
	MissingFields  []string `json:"missing_color_fields,omitempty"`
}

// ValidateTracks audits colour metadata for every video track. The archived
// payload is never rewritten: an absent Colour element or a partial field
// set either fails the plan (PolicyReject) or is reported while the raw
// bytes are retained (PolicyRetain).
func ValidateTracks(tracks []*model.Track, policy RetentionPolicy) ([]ColorReport, []*model.ArchiveError, error) {
	reports := make([]ColorReport, 0, len(tracks))
	var warnings []*model.ArchiveError
	for _, tr := range tracks {
		if tr.Kind != model.KindVideo || tr.Video == nil {
			continue
		}
		v := tr.Video
		report := ColorReport{
			SourceTrackUID: tr.UID,
			ColorPresent:   v.ColorPresent,
			Matrix:         v.MatrixCoefficients,
			Range:          v.ColorRange,
			Primaries:      v.ColorPrimaries,
			Transfer:       v.TransferCharacteristics,
			MissingFields:  append([]string(nil), v.MissingColorFields...),
		}
		reports = append(reports, report)
		if !v.ColorPresent {
			w := model.NewWarning("COLOR_METADATA_MISSING",
				fmt.Sprintf("video track %d has no Colour element; retaining pixels without rewriting", tr.UID),
				tr.HeaderOffset)
			if policy == PolicyReject {
				return nil, nil, w
			}
			warnings = append(warnings, w)
			continue
		}
		if len(v.MissingColorFields) > 0 {
			w := model.NewWarning("COLOR_FIELDS_INCOMPLETE",
				fmt.Sprintf("video track %d Colour lacks %v; raw fields retained unchanged",
					tr.UID, v.MissingColorFields), tr.HeaderOffset)
			if policy == PolicyReject {
				return nil, nil, w
			}
			warnings = append(warnings, w)
		}
	}
	return reports, warnings, nil
}
