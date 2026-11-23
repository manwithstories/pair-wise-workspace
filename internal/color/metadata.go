// Package color normalizes color metadata from camera config and bitstream,
// resolving conflicts per camera position and recording their provenance.
package color

import "fmt"

// Metadata is a set of color descriptors from one source.
type Metadata struct {
	Primaries string // e.g. "bt709", "bt2020"
	Transfer  string // e.g. "bt709", "pq", "hlg"
	Matrix    string // e.g. "bt709", "bt2020nc"
	Range     string // "limited" or "full"
	Source    string // "camera-config", "bitstream", "default"
}

// Conflict records one field where sources disagreed.
type Conflict struct {
	Field     string
	Camera    string
	Bitstream string
	Resolved  string
	Reason    string
}

func (c Conflict) String() string {
	return fmt.Sprintf("%s: camera=%q bitstream=%q -> %q (%s)",
		c.Field, c.Camera, c.Bitstream, c.Resolved, c.Reason)
}

// defaults are applied when neither source provides a value.
var defaults = Metadata{
	Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", Range: "limited", Source: "default",
}

// Resolve merges bitstream metadata with the per-camera configuration.
// Camera config wins on conflict (it reflects the physical position's
// calibration); every conflict is recorded with its source and reason.
func Resolve(camera, bitstream Metadata) (Metadata, []Conflict) {
	out := defaults
	var conflicts []Conflict
	resolve := func(field, cam, bs string) string {
		switch {
		case cam != "" && bs != "" && cam != bs:
			conflicts = append(conflicts, Conflict{
				Field: field, Camera: cam, Bitstream: bs, Resolved: cam,
				Reason: "camera position config overrides bitstream",
			})
			return cam
		case cam != "":
			return cam
		case bs != "":
			return bs
		}
		switch field {
		case "primaries":
			return defaults.Primaries
		case "transfer":
			return defaults.Transfer
		case "matrix":
			return defaults.Matrix
		}
		return defaults.Range
	}
	out.Primaries = resolve("primaries", camera.Primaries, bitstream.Primaries)
	out.Transfer = resolve("transfer", camera.Transfer, bitstream.Transfer)
	out.Matrix = resolve("matrix", camera.Matrix, bitstream.Matrix)
	out.Range = resolve("range", camera.Range, bitstream.Range)
	switch {
	case camera.Source != "":
		out.Source = camera.Source
	case bitstream.Source != "":
		out.Source = bitstream.Source
	}
	return out, conflicts
}
