package adapter

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"mediacore/internal/media"
)

// Adapter converts one container flavour into the unified media model.
// Version is reported so the mapping and the manifest can pin exactly which
// parser produced a result.
type Adapter interface {
	Format() string
	Version() string
	// Detect inspects the initial bytes of a source. It must not consume the
	// source beyond the slice it is handed.
	Detect(head []byte, size int64) bool
	Parse(ctx context.Context, src media.Source) (*media.Container, error)
}

// Registry holds at most one adapter per (format, version) pair.
type Registry struct {
	byFormat map[string][]Adapter
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byFormat: make(map[string][]Adapter, 2)}
}

// Register adds an adapter. Registering the same format and version twice is
// an error rather than a silent replacement.
func (r *Registry) Register(a Adapter) error {
	if a.Format() == "" {
		return media.NewError(media.ClassUsage, "adapter has empty format")
	}
	if _, err := parseVersion(a.Version()); err != nil {
		return media.NewError(media.ClassUsage, "adapter %s has invalid version %q", a.Format(), a.Version())
	}
	for _, existing := range r.byFormat[a.Format()] {
		if existing.Version() == a.Version() {
			return media.NewError(media.ClassUsage,
				"adapter %s version %s is already registered", a.Format(), a.Version())
		}
	}
	r.byFormat[a.Format()] = append(r.byFormat[a.Format()], a)
	sort.SliceStable(r.byFormat[a.Format()], func(i, j int) bool {
		return compareVersions(r.byFormat[a.Format()][i].Version(), r.byFormat[a.Format()][j].Version()) > 0
	})
	return nil
}

// Select returns the adapter for a format. An empty version selects the
// highest registered version, which keeps the choice deterministic.
func (r *Registry) Select(format, version string) (Adapter, error) {
	list := r.byFormat[format]
	if len(list) == 0 {
		return nil, media.NewError(media.ClassUnsupportedCodec, "no adapter registered for format %q", format)
	}
	if version == "" {
		return list[0], nil
	}
	for _, a := range list {
		if a.Version() == version {
			return a, nil
		}
	}
	return nil, media.NewError(media.ClassUnsupportedCodec,
		"format %q has no adapter version %q", format, version)
}

// Detect returns the first adapter (in format order) that claims the source.
func (r *Registry) Detect(src media.Source) (Adapter, error) {
	head := make([]byte, 16)
	n, err := src.ReadAt(head, 0)
	if err != nil && n == 0 {
		return nil, media.NewError(media.ClassTruncated, "input is empty").
			WithOffset(0).WithTolerance(0).WithCause(err)
	}
	head = head[:n]
	formats := make([]string, 0, len(r.byFormat))
	for f := range r.byFormat {
		formats = append(formats, f)
	}
	sort.Strings(formats)
	for _, f := range formats {
		for _, a := range r.byFormat[f] {
			if a.Detect(head, src.Size()) {
				return a, nil
			}
		}
	}
	return nil, media.NewError(media.ClassUnsupportedCodec,
		"input %s is neither a supported MP4 nor the declared MXF subset", src.Name())
}

// FormatForPath returns the adapter format implied by a file extension, or
// "" when the extension carries no hint.
func FormatForPath(path string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".mp4"), strings.HasSuffix(lower, ".m4v"), strings.HasSuffix(lower, ".mov"):
		return "mp4"
	case strings.HasSuffix(lower, ".mxf"):
		return "mxf"
	default:
		return ""
	}
}

// Mapping is one source track's place in the delivery output.
type Mapping struct {
	Source      string          `json:"source"`
	SourceIndex int             `json:"source_index"`
	TrackID     uint32          `json:"track_id"`
	TrackKey    string          `json:"track_key"`
	Type        media.MediaType `json:"type"`
	OutputID    uint32          `json:"output_id"`
	Adapter     string          `json:"adapter"`
	Version     string          `json:"adapter_version"`
}

// Plan is the complete, ordered mapping for a delivery. It deliberately has
// no map fields so re-running it on the same inputs produces byte-identical
// JSON.
type Plan struct {
	Entries     []Mapping `json:"entries"`
	Fingerprint string    `json:"fingerprint"`
}

// BuildPlan maps every track of every container. Tracks are ordered by
// source name, media type and stable identifier, and output IDs are assigned
// in that order. Missing or duplicate identities abort the plan.
func BuildPlan(containers []*media.Container, adapters map[string]Adapter) (*Plan, error) {
	all := make([]*media.Track, 0, 8)
	formatOf := make(map[media.TrackID]string, 8)
	for _, c := range containers {
		if err := media.ValidateIdentity(c.Tracks); err != nil {
			return nil, err
		}
		seen := make(map[uint32]bool, len(c.Tracks))
		for _, t := range c.Tracks {
			if seen[t.ID.Value] {
				return nil, media.NewError(media.ClassDuplicateTrackID,
					"container %s repeats track %d", c.Path, t.ID.Value).
					WithContainer(c.Format).WithTrack(t.ID.Value)
			}
			seen[t.ID.Value] = true
			t.SourcePath = c.Path
			formatOf[t.ID] = c.Format
			all = append(all, t)
		}
	}
	// Deterministic order: source path, then media type, then stable ID.
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.SourcePath != b.SourcePath {
			return a.SourcePath < b.SourcePath
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.ID.Value < b.ID.Value
	})

	plan := &Plan{}
	for i, t := range all {
		format := formatOf[t.ID]
		ad, ok := adapters[format]
		if !ok {
			return nil, media.NewError(media.ClassUnsupportedCodec, "no adapter for format %q", format)
		}
		plan.Entries = append(plan.Entries, Mapping{
			Source:      t.SourcePath,
			SourceIndex: i,
			TrackID:     t.ID.Value,
			TrackKey:    t.ID.String(),
			Type:        t.Type,
			OutputID:    uint32(i + 1),
			Adapter:     format,
			Version:     ad.Version(),
		})
	}
	return plan, nil
}

// Outputs returns key to output-ID for the muxer.
func (p *Plan) Outputs() map[string]uint32 {
	out := make(map[string]uint32, len(p.Entries))
	for _, e := range p.Entries {
		out[e.TrackKey] = e.OutputID
	}
	return out
}

// Sign returns a stable textual signature of a plan for checkpointing.
func (p *Plan) Sign() string {
	var b strings.Builder
	for _, e := range p.Entries {
		fmt.Fprintf(&b, "%s|%s|%s|%d;", e.Source, e.TrackKey, e.Type, e.OutputID)
	}
	return b.String()
}

func parseVersion(v string) ([3]int, error) {
	var out [3]int
	parts := strings.SplitN(v, ".", 3)
	if len(parts) == 0 || len(parts) > 3 {
		return out, fmt.Errorf("bad version")
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, fmt.Errorf("bad version segment")
		}
		out[i] = n
	}
	return out, nil
}

func compareVersions(a, b string) int {
	av, errA := parseVersion(a)
	bv, errB := parseVersion(b)
	if errA != nil || errB != nil {
		return strings.Compare(a, b)
	}
	for i := 0; i < 3; i++ {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// DefaultRegistry builds the registry used by the CLI. It is a function, not
// a package variable, so tests get a fresh registry and cannot be affected by
// registration order elsewhere.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	_ = r.Register(&MP4Adapter{})
	_ = r.Register(&MXFAdapter{})
	return r
}
