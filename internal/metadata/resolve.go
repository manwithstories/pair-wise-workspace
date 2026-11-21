package metadata

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mediamerge/internal/fault"
)

// ResolveOptions are the delivery targets applied after parsing.
type ResolveOptions struct {
	// Explicit colour overrides the container and track descriptors.
	ExplicitColor *ColorDesc
	// RequiredColorFields lists fields that must resolve; empty means all.
	RequiredColorFields []string
}

// Resolve parses a file by extension and normalizes every track. All
// diagnostics from parsing, colour resolution, rotation and crop land in one
// report so a single terminal run shows every problem.
func Resolve(path string, opts ResolveOptions) (*File, error) {
	var (
		f   *File
		err error
	)
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mxf":
		f, err = ParseMXF(path)
	case ".mp4", ".m4v", ".mov":
		f, err = ParseMP4(path)
	default:
		return nil, fault.New(fault.StageParse, fault.CodeInvalidArgument,
			fmt.Sprintf("unsupported container %q for %s", filepath.Ext(path), path))
	}
	if err != nil {
		return nil, err
	}
	if err := NormalizeFile(f, opts); err != nil {
		return f, err
	}
	return f, nil
}

// NormalizeFile normalizes each track in place and returns the combined
// error, if any. Tracks are processed in ID order so the report is
// deterministic regardless of box order.
func NormalizeFile(f *File, opts ResolveOptions) error {
	sort.SliceStable(f.Tracks, func(i, j int) bool { return f.Tracks[i].ID < f.Tracks[j].ID })
	for _, tr := range f.Tracks {
		explicit := opts.ExplicitColor
		if _, err := tr.Normalize(&f.Diagnostics, f.Color, explicit); err != nil {
			return err
		}
	}
	if len(opts.RequiredColorFields) > 0 {
		if err := requireColorFields(f, opts.RequiredColorFields); err != nil {
			return err
		}
	}
	return f.Diagnostics.AsError(fault.StageResolve)
}

// requireColorFields reports a missing delivery field even when colour
// resolution was skipped (audio-only files).
func requireColorFields(f *File, fields []string) error {
	seen := map[string]bool{}
	for _, tr := range f.Tracks {
		if tr.Kind != KindVideo || tr.Resolved == nil {
			continue
		}
		c := tr.Resolved
		for _, name := range fields {
			if seen[name] {
				continue
			}
			present := true
			switch name {
			case "primaries":
				present = c.Primaries != 0
			case "transfer":
				present = c.Transfer != 0
			case "matrix":
				present = c.Matrix != 0
			case "range":
				present = true
			default:
				return fault.New(fault.StageResolve, fault.CodeInvalidArgument,
					"unknown required colour field "+name)
			}
			if !present {
				f.Diagnostics.Add(fault.Diagnostic{
					Code: fault.CodeColorMissing, Severity: fault.SeverityError,
					Stage: fault.StageResolve, TrackID: tr.ID, Field: name,
					Message: "required delivery field is unresolved",
				})
				seen[name] = true
			}
		}
	}
	return nil
}

// FindFiles returns generated fixture paths of the given extensions in dir,
// sorted for deterministic iteration.
func FindFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".mxf", ".mp4":
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}
