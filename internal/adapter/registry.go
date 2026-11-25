// Package adapter turns container segments (MPEG-TS, CMAF/fMP4) into unified
// media samples. Adapters are versioned so a container revision can be added
// without changing callers.
package adapter

import (
	"fmt"
	"sort"
	"strings"

	"mediad/internal/media"
)

// Container identifies a segmentation format.
type Container string

const (
	ContainerCMAF Container = "cmaf"
	ContainerTS   Container = "ts"
)

// InitTrack is one track discovered in an initialization segment.
type InitTrack struct {
	ID          uint32     `json:"id"`
	Kind        media.Kind `json:"kind"`
	Codec       string     `json:"codec"` // normalized fourcc, e.g. "avc1"
	EntryType   string     `json:"entry_type"`
	Timescale   int64      `json:"timescale"`
	SampleEntry []byte     `json:"-"` // raw stsd child, preserved byte-for-byte
	Width       int        `json:"width,omitempty"`
	Height      int        `json:"height,omitempty"`
}

// InitSegment is the parsed initialization data for a rendition.
type InitSegment struct {
	Raw    []byte       `json:"-"`
	Tracks []*InitTrack `json:"tracks"`
}

// TrackByID returns the init track with the given container ID.
func (i *InitSegment) TrackByID(id uint32) *InitTrack {
	for _, t := range i.Tracks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// SegmentRequest carries one segment to an adapter. Refs holds the caller's
// last unwrapped decode time per track ID, so 32/33-bit bases can be expanded
// consistently across segment boundaries.
type SegmentRequest struct {
	Data         []byte
	Pos          media.SourcePos
	SegmentIndex int
	URI          string
	Init         *InitSegment
	Keys         map[uint32]media.TrackKey
	Refs         map[uint32]int64
	Limits       media.Limits
}

// Adapter decodes one container version.
type Adapter interface {
	Kind() Container
	Version() int
	ParseInit(data []byte, pos media.SourcePos) (*InitSegment, error)
	ParseSegment(req SegmentRequest) ([]media.Sample, error)
}

// Registry resolves a (container, version) pair to an adapter.
type Registry struct {
	adapters map[string]Adapter
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{adapters: make(map[string]Adapter)}
}

func regKey(kind Container, version int) string {
	return fmt.Sprintf("%s@v%d", kind, version)
}

// Register adds an adapter, replacing any previous one for the same key.
func (r *Registry) Register(a Adapter) {
	r.adapters[regKey(a.Kind(), a.Version())] = a
}

// Get resolves an adapter or returns a classified unsupported error.
func (r *Registry) Get(kind Container, version int) (Adapter, error) {
	a, ok := r.adapters[regKey(kind, version)]
	if !ok {
		return nil, media.New(media.CodeUnsupported, "adapter", media.SourcePos{},
			"no adapter for container %s version %d", kind, version)
	}
	return a, nil
}

// DefaultRegistry returns a registry with the shipped container adapters.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(&CMAFV1{})
	r.Register(&TSV1{})
	return r
}

// NormalizeCodec reduces a manifest or sample-entry codec string to its
// four-character family ("avc1.640028" -> "avc1", "hev1" -> "hev1").
func NormalizeCodec(codec string) string {
	codec = strings.TrimSpace(strings.ToLower(codec))
	if codec == "" {
		return ""
	}
	if i := strings.IndexByte(codec, '.'); i > 0 {
		codec = codec[:i]
	}
	return codec
}

// RenditionDesc is the non-container identity of one manifest rendition.
type RenditionDesc struct {
	Camera   string
	Language string
	Role     string
	Codec    string
	Type     string // "video" or "audio"
}

// MapTracks assigns a TrackKey to every init track of one rendition and
// rejects key conflicts. The mapping is: camera/language from the rendition,
// role from the rendition for video and "audio" for audio tracks, and the
// codec family from the sample entry.
func MapTracks(init *InitSegment, desc RenditionDesc) (map[uint32]media.TrackKey, error) {
	if init == nil || len(init.Tracks) == 0 {
		return nil, media.New(media.CodeNoTrack, "map-tracks", media.SourcePos{},
			"camera %s has no tracks", desc.Camera)
	}
	out := make(map[uint32]media.TrackKey, len(init.Tracks))
	used := make(map[media.TrackKey]uint32, len(init.Tracks))
	for _, t := range init.Tracks {
		role := desc.Role
		if t.Kind == media.KindAudio {
			role = "audio"
		}
		if role == "" {
			role = "main"
		}
		codec := NormalizeCodec(t.Codec)
		key := media.TrackKey{
			Camera:   desc.Camera,
			Language: defaultString(desc.Language, "und"),
			Codec:    codec,
			Role:     role,
		}
		if prev, ok := used[key]; ok {
			return nil, media.New(media.CodeTrackConflict, "map-tracks", media.SourcePos{},
				"tracks %d and %d both map to %s", prev, t.ID, key)
		}
		used[key] = t.ID
		out[t.ID] = key
	}
	return out, nil
}

// RequiredKeys returns the keys a rendition must produce, as inferred from the
// manifest codec and rendition type. Codec is a family match.
func RequiredKeys(desc RenditionDesc) []media.TrackKey {
	codec := NormalizeCodec(desc.Codec)
	role := desc.Role
	if desc.Type == "audio" || (codec != "" && strings.HasPrefix(codec, "mp4a")) {
		role = "audio"
	}
	if role == "" {
		role = "main"
	}
	return []media.TrackKey{{
		Camera:   desc.Camera,
		Language: defaultString(desc.Language, "und"),
		Codec:    codec,
		Role:     role,
	}}
}

// CheckRequired verifies every required key is present in mapped, ignoring
// codec when the manifest did not declare one.
func CheckRequired(mapped map[uint32]media.TrackKey, required []media.TrackKey, pos media.SourcePos) error {
	present := make([]media.TrackKey, 0, len(mapped))
	for _, k := range mapped {
		present = append(present, k)
	}
	for _, want := range required {
		found := false
		for _, got := range present {
			if got.Camera != want.Camera || got.Language != want.Language || got.Role != want.Role {
				continue
			}
			if want.Codec == "" || got.Codec == want.Codec {
				found = true
				break
			}
		}
		if !found {
			return media.New(media.CodeRequiredTrack, "required-track", pos,
				"required track %s is missing", want)
		}
	}
	return nil
}

// TrackSet detects cross-source key collisions before output is submitted.
type TrackSet struct {
	owner map[media.TrackKey]string
}

// NewTrackSet returns an empty set.
func NewTrackSet() *TrackSet { return &TrackSet{owner: make(map[media.TrackKey]string)} }

// Add registers keys for a source label, rejecting duplicates across sources.
func (s *TrackSet) Add(label string, keys map[uint32]media.TrackKey) error {
	ordered := make([]media.TrackKey, 0, len(keys))
	for _, k := range keys {
		ordered = append(ordered, k)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Less(ordered[j]) })
	for _, k := range ordered {
		if prev, ok := s.owner[k]; ok {
			return media.New(media.CodeTrackConflict, "track-set", media.SourcePos{},
				"key %s produced by both %s and %s", k, prev, label)
		}
		s.owner[k] = label
	}
	return nil
}

// Keys returns the sorted keys registered so far.
func (s *TrackSet) Keys() []media.TrackKey {
	out := make([]media.TrackKey, 0, len(s.owner))
	for k := range s.owner {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out
}

func defaultString(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
