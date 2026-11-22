// Package manifest loads and validates the archive intake list. The manifest
// is the only source of truth about which files are processed and how tracks
// map onto the delivery profile; nothing is discovered by scanning the file
// system.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediabridge/internal/fault"
	"mediabridge/internal/media"
	"mediabridge/internal/safeio"
)

// Document is the on-disk manifest.
type Document struct {
	// Version is the manifest schema version.
	Version int `json:"version"`
	// RootDir confines every referenced path. Relative references resolve
	// against it; absolute references must still fall inside it.
	RootDir string `json:"root_dir"`
	// Assets lists intake units. Each asset has exactly one primary container
	// and optional auxiliary RTP dumps.
	Assets []Asset `json:"assets"`
}

// Asset is one archival intake unit.
type Asset struct {
	ID string `json:"id"`
	// Primary is the MP4/MOV container.
	Primary Source `json:"primary"`
	// Auxiliary holds RTP dumps to merge as auxiliary tracks.
	Auxiliary []Source `json:"auxiliary,omitempty"`
	// Mapping selects how tracks map onto the delivery profile.
	Mapping Mapping `json:"mapping"`
}

// Source references one file.
type Source struct {
	// Format is the adapter format key ("mp4" or "rtp"). Empty means sniff.
	Format string `json:"format"`
	// Path is relative to the manifest root (or absolute inside it).
	Path string `json:"path"`
	// Version pins the adapter version; 0 means latest.
	Version int `json:"version,omitempty"`
	// SampleRate is the RTP clock rate (RTP sources only).
	SampleRate uint32 `json:"sample_rate,omitempty"`
	// TrackID overrides the RTP SSRC as the logical track id.
	TrackID uint32 `json:"track_id,omitempty"`
}

// Mapping describes the delivery-profile track layout.
type Mapping struct {
	// VideoTrackID selects the primary video track (0 = first video).
	VideoTrackID uint32 `json:"video_track_id,omitempty"`
	// AudioTrackID selects the primary audio track (0 = first audio).
	AudioTrackID uint32 `json:"audio_track_id,omitempty"`
	// Auxiliary lists auxiliary tracks to carry. Each entry names a source
	// (RTP path) or an MP4 track id.
	Auxiliary []AuxMapping `json:"auxiliary,omitempty"`
	// DropUnmapped discards tracks not named in the mapping. When false an
	// unmapped track is a fatal mapping error.
	DropUnmapped bool `json:"drop_unmapped"`
}

// AuxMapping binds one auxiliary source/track to a delivery slot.
type AuxMapping struct {
	// Name is the delivery-profile slot name (e.g. "timecode", "klv").
	Name string `json:"name"`
	// Source is the manifest path of the RTP dump this slot draws from, or
	// empty when drawing from an MP4 auxiliary track.
	Source string `json:"source,omitempty"`
	// MPTrackID selects an MP4 track by id when Source is empty.
	MPTrackID uint32 `json:"mp4_track_id,omitempty"`
}

// Loaded is a validated manifest with resolved absolute paths.
type Loaded struct {
	Doc    *Document
	root   string
	assets []ResolvedAsset
}

// ResolvedAsset pairs the manifest asset with its confined paths.
type ResolvedAsset struct {
	Asset
	PrimaryPath    string
	AuxiliaryPaths map[string]string // manifest-relative path → absolute
}

// Load reads, parses and resolves a manifest under its declared root.
func Load(path string) (*Loaded, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fault.New(fault.CodePath, fault.Location{SourcePath: path},
			"read manifest failed").Wrap(err).WithOp("manifest.Load")
	}
	var doc Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fault.New(fault.CodeCorruptContainer, fault.Location{SourcePath: path},
			"manifest parse failed: %v", err).WithOp("manifest.Load")
	}
	// Resolve the root relative to the manifest location when it is relative.
	root := doc.RootDir
	if root == "" {
		root = filepath.Dir(path)
	} else if !filepath.IsAbs(root) {
		root = filepath.Join(filepath.Dir(path), root)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fault.New(fault.CodePath, fault.Location{SourcePath: path},
			"resolve root failed").Wrap(err).WithOp("manifest.Load")
	}

	l := &Loaded{Doc: &doc, root: absRoot}
	for i := range doc.Assets {
		a := doc.Assets[i]
		if a.ID == "" {
			return nil, fault.New(fault.CodeMapping, fault.Location{SourcePath: path},
				"asset %d has no id", i).WithOp("manifest.Load")
		}
		ra := ResolvedAsset{Asset: a, AuxiliaryPaths: map[string]string{}}
		pp, err := resolveSource(absRoot, a.Primary)
		if err != nil {
			return nil, withAsset(err, a.ID, path)
		}
		ra.PrimaryPath = pp
		for _, aux := range a.Auxiliary {
			ap, err := resolveSource(absRoot, aux)
			if err != nil {
				return nil, withAsset(err, a.ID, path)
			}
			ra.AuxiliaryPaths[aux.Path] = ap
		}
		if err := validateMapping(a.Mapping, a.Auxiliary); err != nil {
			return nil, withAsset(err, a.ID, path)
		}
		l.assets = append(l.assets, ra)
	}
	return l, nil
}

func withAsset(err error, assetID, path string) error {
	if e, ok := fault.As(err); ok {
		loc := e.Loc
		loc.AssetID = assetID
		if loc.SourcePath == "" {
			loc.SourcePath = path
		}
		e.WithLocation(loc)
	}
	return err
}

func resolveSource(root string, src Source) (string, error) {
	if src.Path == "" {
		return "", fault.New(fault.CodePath, fault.Location{}, "source has empty path").
			WithOp("manifest.Resolve")
	}
	// Manifest paths may be absolute; they must still be inside root.
	var resolved string
	var err error
	if filepath.IsAbs(src.Path) {
		var rel string
		rel, err = relInside(root, src.Path)
		if err == nil {
			resolved, err = safeio.Resolve(root, rel)
		}
	} else {
		resolved, err = safeio.Resolve(root, src.Path)
	}
	if err != nil {
		return "", fault.New(fault.CodePath, fault.Location{SourcePath: src.Path},
			"path rejected").Wrap(err).WithOp("manifest.Resolve")
	}
	return resolved, nil
}

// relInside converts an absolute path into a root-relative one, rejecting
// paths outside the root.
func relInside(root, abs string) (string, error) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", fault.New(fault.CodePath, fault.Location{SourcePath: abs},
			"path not under root").Wrap(err)
	}
	return rel, nil
}

func validateMapping(m Mapping, aux []Source) error {
	seenAux := map[string]bool{}
	for _, a := range m.Auxiliary {
		if a.Name == "" {
			return fault.New(fault.CodeMapping, fault.Location{},
				"auxiliary mapping has empty name").WithOp("manifest.Validate")
		}
		if seenAux[a.Name] {
			return fault.New(fault.CodeDuplicateMapping, fault.Location{},
				"duplicate auxiliary mapping %q", a.Name).WithOp("manifest.Validate")
		}
		seenAux[a.Name] = true
		if a.Source != "" {
			found := false
			for _, s := range aux {
				if s.Path == a.Source {
					found = true
					break
				}
			}
			if !found {
				return fault.New(fault.CodeMissingMapping, fault.Location{SourcePath: a.Source},
					"auxiliary mapping %q references unknown source", a.Name).WithOp("manifest.Validate")
			}
		}
	}
	return nil
}

// Root returns the confined root directory.
func (l *Loaded) Root() string { return l.root }

// Assets returns the resolved assets.
func (l *Loaded) Assets() []ResolvedAsset { return l.assets }

// ResolvePath confines an arbitrary path against the manifest root. Used by
// the CLI when a single manifest is not supplied.
func (l *Loaded) ResolvePath(p string) (string, error) {
	if filepath.IsAbs(p) {
		rel, err := relInside(l.root, p)
		if err != nil {
			return "", err
		}
		return safeio.Resolve(l.root, rel)
	}
	return safeio.Resolve(l.root, p)
}

// TrackKindFor maps a handler/type string to a kernel track kind.
func TrackKindFor(handler string) media.TrackKind {
	switch handler {
	case "vide":
		return media.KindVideo
	case "soun":
		return media.KindAudio
	default:
		return media.KindAux
	}
}

var _ = fmt.Sprintf
