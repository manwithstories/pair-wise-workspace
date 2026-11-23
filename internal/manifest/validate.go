// Manifest validation: clock overflow, edit-run bounds and path confinement.
package manifest

import (
	"fmt"
	"path/filepath"
	"strings"

	"archive.local/archive/internal/model"
)

func (m *Manifest) validate() error {
	for i := range m.Entries {
		e := &m.Entries[i]
		if len(e.Edit) == 0 {
			e.Edit = []Edit{{Line: e.Line, MediaTime: e.OriginTicks, SegmentDuration: -1, Rate: 65536}}
		}
		if e.Duration >= 0 && e.OriginTicks > 0 && e.Duration > (1<<62)-e.OriginTicks {
			return &model.Error{Code: model.CodeTimescaleOverflow, File: m.Path, Line: e.Line,
				Track: e.TrackID, Sample: -1, OffsetBytes: -1,
				Detail: "origin plus duration overflows the media clock"}
		}
		for _, ed := range e.Edit {
			if ed.MediaTime < 0 {
				// Empty edit: an explicit presentation gap, no media consumed.
				if ed.MediaTime != -1 {
					return &model.Error{Code: model.CodeEditListRange, File: m.Path, Line: ed.Line,
						Track: e.TrackID, Sample: -1, OffsetBytes: -1,
						Detail: "empty edit must use media time -1"}
				}
				continue
			}
			if ed.SegmentDuration < 0 {
				continue // implicit "rest of media"
			}
			// Edit runs are expressed in media ticks and must address media
			// that actually exists on the track.
			if e.Duration >= 0 && ed.MediaTime > e.Duration ||
				e.Duration >= 0 && ed.SegmentDuration > e.Duration-ed.MediaTime {
				return &model.Error{Code: model.CodeEditListRange, File: m.Path, Line: ed.Line,
					Track: e.TrackID, Sample: -1, OffsetBytes: -1,
					Detail: fmt.Sprintf("edit [%d,%d) exceeds declared media duration %d",
						ed.MediaTime, ed.MediaTime+ed.SegmentDuration, e.Duration)}
			}
			if ed.MediaTime > (1<<62)-ed.SegmentDuration {
				return &model.Error{Code: model.CodeTimescaleOverflow, File: m.Path, Line: ed.Line,
					Track: e.TrackID, Sample: -1, OffsetBytes: -1,
					Detail: "edit end overflows the media clock"}
			}
		}
	}
	return nil
}

// resolve maps a manifest-relative path into an absolute one without leaving
// the manifest directory. Failures name the offending manifest line and track.
func (m *Manifest) resolve(line int, track uint32, src string) (string, error) {
	if filepath.IsAbs(src) || filepath.VolumeName(src) != "" {
		return "", m.referenceError(line, track, "source must be relative to the manifest directory")
	}
	root, err := filepath.Abs(m.Dir)
	if err != nil {
		return "", m.referenceError(line, track, "cannot resolve manifest directory")
	}
	abs, err := filepath.Abs(filepath.Join(root, src))
	if err != nil {
		return "", m.referenceError(line, track, "cannot resolve source path")
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", m.referenceError(line, track, "source escapes the working directory")
	}
	return abs, nil
}

// ResolveSource returns the absolute path for a track's media file.
func (m *Manifest) ResolveSource(e Entry) (string, error) {
	return m.resolve(e.Line, e.TrackID, e.Source)
}

func (m *Manifest) syntaxError(line int, detail string) *model.Error {
	return &model.Error{Code: model.CodeManifestSyntax, File: m.Path, Line: line, Sample: -1, OffsetBytes: -1,
		Detail: detail}
}

func (m *Manifest) referenceError(line int, track uint32, detail string) *model.Error {
	return &model.Error{Code: model.CodeManifestReference, File: m.Path, Line: line, Track: track,
		Sample: -1, OffsetBytes: -1, Detail: detail}
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
