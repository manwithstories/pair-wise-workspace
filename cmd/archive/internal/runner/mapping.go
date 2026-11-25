package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"mediaarchive/internal/demux"
	"mediaarchive/internal/model"
	"mediaarchive/internal/track"
)

// Options carries every CLI knob into the orchestration.
type Options struct {
	Input      string
	Output     string
	Manifest   string
	Checkpoint string
	MapFile    string
	Workers    int
	Queue      int
	BufferMem  int64
	TargetBPS  int64
	OnDamage   string // "fail" or "recover"
	StopAfter  int    // stop after this many samples; <=0 disables
	Resume     bool
}

type sidecarRule struct {
	TrackID uint32 `json:"track_id"`
	Role    string `json:"role"`
}

// defaultMapPath returns the sidecar path produced alongside a fixture.
func defaultMapPath(input string) string {
	ext := filepath.Ext(input)
	return strings.TrimSuffix(input, ext) + ".map.json"
}

// loadMapping reads explicit rules. Without a --map flag the input's sidecar
// is used; if that is absent every non-hint track is mapped by its declared
// kind. A present sidecar is always validated strictly.
func loadMapping(c *demux.Container, mapFile string) ([]track.Assignment, error) {
	path := mapFile
	if path == "" {
		path = defaultMapPath(c.Path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if mapFile != "" {
			return nil, model.NewError(model.StageTrack, "cannot read mapping file: "+path).Wrap(err)
		}
		return declaredMapping(c), nil
	}
	var rules []sidecarRule
	if err := json.Unmarshal(b, &rules); err != nil {
		return nil, model.NewError(model.StageTrack, "invalid mapping json: "+path).Wrap(err)
	}
	out := make([]track.Assignment, 0, len(rules))
	for _, r := range rules {
		out = append(out, track.Assignment{TrackID: r.TrackID, Role: r.Role})
	}
	return out, nil
}

// declaredMapping maps each non-hint track by the kind the container itself
// declares. It is deterministic and never silently drops a track.
func declaredMapping(c *demux.Container) []track.Assignment {
	var out []track.Assignment
	for _, t := range c.Tracks {
		if t.Kind == demux.KindHint {
			continue
		}
		role := track.RoleAudio
		if t.Kind == demux.KindVideo {
			role = track.RoleVideo
		}
		out = append(out, track.Assignment{TrackID: t.ID, Role: role})
	}
	return out
}
