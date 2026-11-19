package render

import (
	"encoding/json"

	"pairflow/media/internal/timeline"
)

// marshalManifest renders the manifest as pretty JSON with a trailing
// newline, and never includes time-varying data.
func marshalManifest(m *Manifest) ([]byte, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

var _ = timeline.KindVideo
