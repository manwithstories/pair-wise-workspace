package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"bq/internal/model"
)

// Parse decodes a manifest from raw JSON bytes. The byte offset of syntax
// failures is recovered from the JSON decoder so corrupt-list records point
// at the offending input position.
func Parse(data []byte) (*Manifest, []model.Error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var m Manifest
	if err := dec.Decode(&m); err != nil {
		offset := decodeOffset(err)
		if offset < 0 {
			offset = int64(len(data)) // io.ErrUnexpectedEOF carries no offset
		}
		rec := model.NewError(model.StageManifest, 0, model.ErrCorruptManifest,
			model.SeverityFatal, "json decode failed: "+err.Error(), offset, 0, false)
		return nil, []model.Error{rec}
	}
	if dec.More() {
		rec := model.NewError(model.StageManifest, 0, model.ErrCorruptManifest,
			model.SeverityFatal, "trailing data after manifest document", int64(len(data)), 0, false)
		return nil, []model.Error{rec}
	}

	if errs := Validate(&m); len(errs) > 0 {
		return nil, errs
	}
	return &m, nil
}

// ParseFile loads and parses a manifest path.
func ParseFile(path string) (*Manifest, []model.Error) {
	info, err := os.Stat(path)
	if err != nil {
		rec := model.NewError(model.StageManifest, 0, model.ErrCorruptManifest,
			model.SeverityFatal, "manifest not readable: "+err.Error(), -1, 0, false)
		return nil, []model.Error{rec}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		rec := model.NewError(model.StageManifest, 0, model.ErrCorruptManifest,
			model.SeverityFatal, "manifest read failed: "+err.Error(), info.Size(), 0, false)
		return nil, []model.Error{rec}
	}
	return Parse(data)
}

// decodeOffset extracts the byte position from a json syntax error.
func decodeOffset(err error) int64 {
	var se *json.SyntaxError
	if ok := asJSONError(err, &se); ok {
		return se.Offset
	}
	var te *json.UnmarshalTypeError
	if ok := asJSONError(err, &te); ok {
		return te.Offset
	}
	return -1
}

func asJSONError(err error, target any) bool {
	switch t := target.(type) {
	case **json.SyntaxError:
		if v, ok := err.(*json.SyntaxError); ok {
			*t = v
			return true
		}
	case **json.UnmarshalTypeError:
		if v, ok := err.(*json.UnmarshalTypeError); ok {
			*t = v
			return true
		}
	}
	return false
}
// Validate enforces structural and codec rules without touching the packet
// payloads. It returns fatal records only; callers abort on any error.
func Validate(m *Manifest) []model.Error {
	fatal := func(code, msg string, args ...any) model.Error {
		return model.NewError(model.StageManifest, 0, code, model.SeverityFatal,
			fmt.Sprintf(msg, args...), -1, 0, false)
	}
	switch {
	case m.Version == "":
		return []model.Error{fatal(model.ErrCorruptManifest, "missing version")}
	case m.DurationS <= 0:
		return []model.Error{fatal(model.ErrCorruptManifest, "duration_s must be positive, got %d", m.DurationS)}
	case m.MasterHz <= 0:
		return []model.Error{fatal(model.ErrCorruptManifest, "master_hz must be positive, got %d", m.MasterHz)}
	case len(m.Streams) == 0:
		return []model.Error{fatal(model.ErrCorruptManifest, "manifest declares no streams")}
	}

	seenID := make(map[int]bool)
	videoTracks := 0
	for i := range m.Streams {
		s := &m.Streams[i]
		if seenID[s.ID] {
			return []model.Error{fatal(model.ErrCorruptManifest, "duplicate stream id %d", s.ID)}
		}
		seenID[s.ID] = true
		if s.Kind != KindVideo && s.Kind != KindAudio {
			return []model.Error{fatal(model.ErrUnsupportedFormat, "stream %d unknown kind %q", s.ID, s.Kind)}
		}
		if s.Container != ContainerMP4 {
			return []model.Error{fatal(model.ErrUnsupportedFormat, "stream %d container %q (only mp4)", s.ID, s.Container)}
		}
		if !SupportedCodecs[s.Codec] {
			return []model.Error{fatal(model.ErrUnsupportedFormat, "stream %d codec %q (only h264/aac)", s.ID, s.Codec)}
		}
		if s.Kind == KindVideo && s.Codec != "h264" {
			return []model.Error{fatal(model.ErrUnsupportedFormat, "video stream %d must be h264, got %q", s.ID, s.Codec)}
		}
		if s.Kind == KindAudio && s.Codec != "aac" {
			return []model.Error{fatal(model.ErrUnsupportedFormat, "audio stream %d must be aac, got %q", s.ID, s.Codec)}
		}
		if s.TimebaseNum <= 0 || s.TimebaseDen <= 0 {
			return []model.Error{fatal(model.ErrCorruptManifest, "stream %d invalid timebase %d/%d", s.ID, s.TimebaseNum, s.TimebaseDen)}
		}
		if s.File == "" {
			return []model.Error{fatal(model.ErrCorruptManifest, "stream %d has empty file", s.ID)}
		}
		if s.WrapPeriod < 0 {
			return []model.Error{fatal(model.ErrCorruptManifest, "stream %d negative wrap period", s.ID)}
		}
		if s.Kind == KindVideo {
			videoTracks++
			if s.Width <= 0 || s.Height <= 0 {
				return []model.Error{fatal(model.ErrCorruptManifest, "video stream %d missing dimensions", s.ID)}
			}
			if s.KeyframeEvery <= 0 {
				return []model.Error{fatal(model.ErrCorruptManifest, "video stream %d keyframe_every must be positive", s.ID)}
			}
		}
		for _, ed := range s.EditList {
			if ed.EndPTS <= ed.StartPTS {
				return []model.Error{fatal(model.ErrCorruptManifest, "stream %d edit range empty/inverted", s.ID)}
			}
		}
	}
	if videoTracks < 1 {
		return []model.Error{fatal(model.ErrCorruptManifest, "manifest needs at least one video track")}
	}
	return nil
}
