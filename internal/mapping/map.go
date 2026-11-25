// Package mapping parses and validates elementary-track routing rules.
package mapping

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"mediqc/internal/media"
)

// Route is one explicit source PID to output track mapping.
type Route struct {
	SourcePID uint16          `json:"source_pid"`
	TrackID   int             `json:"track_id"`
	Kind      media.TrackKind `json:"kind"`
	Codec     media.Codec     `json:"codec"`
	Target    string          `json:"target"`
}

// Mapping is an explicit track routing table. It never contains an implicit
// default route.
type Mapping struct {
	Routes []Route `json:"routes"`
}

// ParseJSON reads a JSON mapping document.
func ParseJSON(data []byte) (*Mapping, *media.Error) {
	var m Mapping
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, &media.Error{
			Code: media.CodeMappingInvalid, Component: "mapping",
			Message: fmt.Sprintf("invalid mapping JSON: %v", err),
		}
	}
	return &m, m.StructuralValidate()
}

// ParseSpec parses "pid:id:kind:codec:target,..." routes for the CLI.
func ParseSpec(spec string) (*Mapping, *media.Error) {
	m := &Mapping{}
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return m, nil
	}
	for _, item := range strings.Split(spec, ",") {
		parts := strings.Split(strings.TrimSpace(item), ":")
		if len(parts) < 4 || len(parts) > 5 {
			return nil, &media.Error{Code: media.CodeMappingInvalid, Component: "mapping", Message: "expected pid:id:kind:codec[:target]"}
		}
		pid64, err := strconv.ParseUint(parts[0], 0, 16)
		if err != nil || pid64 > 0x1fff {
			return nil, &media.Error{Code: media.CodeMappingInvalid, Component: "mapping", Message: "PID must be 0..8191"}
		}
		id, err := strconv.Atoi(parts[1])
		if err != nil || id <= 0 {
			return nil, &media.Error{Code: media.CodeMappingInvalid, Component: "mapping", Message: "track id must be positive"}
		}
		target := fmt.Sprintf("track-%d", id)
		if len(parts) == 5 {
			target = parts[4]
		}
		m.Routes = append(m.Routes, Route{
			SourcePID: uint16(pid64), TrackID: id,
			Kind: media.TrackKind(parts[2]), Codec: media.Codec(parts[3]), Target: target,
		})
	}
	return m, m.StructuralValidate()
}

// StructuralValidate checks PID range, codec support and duplicate routes.
func (m *Mapping) StructuralValidate() *media.Error {
	if m == nil {
		return &media.Error{Code: media.CodeMappingInvalid, Component: "mapping", Message: "nil mapping"}
	}
	seenPID := map[uint16]int{}
	seenTrack := map[int]uint16{}
	seenTarget := map[string]uint16{}
	for i, r := range m.Routes {
		if r.SourcePID > 0x1fff {
			return invalid("PID exceeds 13 bits")
		}
		if r.TrackID <= 0 {
			return invalid("target track id must be positive")
		}
		if r.Kind != media.TrackVideo && r.Kind != media.TrackAudio {
			return invalid("kind must be video or audio")
		}
		if !media.SupportedCodec(r.Codec) {
			return &media.Error{
				Code: media.CodeUnsupported, Component: "mapping",
				Message: fmt.Sprintf("codec %q is not supported for pass-through", r.Codec),
			}
		}
		if (r.Kind == media.TrackVideo && r.Codec != media.CodecH264) ||
			(r.Kind == media.TrackAudio && r.Codec != media.CodecAAC) {
			return invalid("kind and codec mismatch")
		}
		if old, ok := seenPID[r.SourcePID]; ok {
			return invalid(fmt.Sprintf("PID %d mapped more than once at routes %d and %d", r.SourcePID, old, i))
		}
		if oldPID, ok := seenTrack[r.TrackID]; ok {
			return invalid(fmt.Sprintf("duplicate target track %d for PIDs %d and %d", r.TrackID, oldPID, r.SourcePID))
		}
		if r.Target != "" {
			if oldPID, ok := seenTarget[r.Target]; ok {
				return invalid(fmt.Sprintf("duplicate target %q for PIDs %d and %d", r.Target, oldPID, r.SourcePID))
			}
			seenTarget[r.Target] = r.SourcePID
		}
		seenPID[r.SourcePID] = i
		seenTrack[r.TrackID] = r.SourcePID
	}
	return nil
}

// ValidateAgainstPMT verifies every explicit route matches the material.
// Unmapped elementary streams are rejected rather than assigned a default.
func (m *Mapping) ValidateAgainstPMT(streams []struct {
	PID   uint16
	Codec media.Codec
	Kind  media.TrackKind
}) ([]media.Track, *media.Error) {
	if err := m.StructuralValidate(); err != nil {
		return nil, err
	}
	byPID := map[uint16]struct {
		Codec media.Codec
		Kind  media.TrackKind
	}{}
	for _, s := range streams {
		byPID[s.PID] = struct {
			Codec media.Codec
			Kind  media.TrackKind
		}{s.Codec, s.Kind}
	}
	tracks := make([]media.Track, 0, len(m.Routes))
	for _, r := range m.Routes {
		actual, ok := byPID[r.SourcePID]
		if !ok {
			return nil, &media.Error{
				Code: media.CodeMappingInvalid, Component: "mapping",
				Message: fmt.Sprintf("mapping references absent PID %d; refusing default track", r.SourcePID),
			}
		}
		if actual.Codec != r.Codec || actual.Kind != r.Kind {
			return nil, &media.Error{
				Code: media.CodeMappingInvalid, Component: "mapping",
				Message: fmt.Sprintf("PID %d is %s/%s but mapping requires %s/%s; refusing silent default",
					r.SourcePID, actual.Kind, actual.Codec, r.Kind, r.Codec),
			}
		}
		tracks = append(tracks, media.Track{
			ID: r.TrackID, PID: r.SourcePID, Kind: r.Kind, Codec: r.Codec,
			Timescale: 90000,
		})
	}
	return tracks, nil
}

// DefaultForStreams creates the only built-in mapping used by generated fixtures.
func DefaultForStreams(streams []struct {
	PID   uint16
	Codec media.Codec
	Kind  media.TrackKind
}) (*Mapping, *media.Error) {
	m := &Mapping{}
	for i, s := range streams {
		if !media.SupportedCodec(s.Codec) {
			return nil, &media.Error{Code: media.CodeUnsupported, Component: "mapping", Message: "refusing unsupported codec"}
		}
		id := i + 1
		target := "video"
		if s.Kind == media.TrackAudio {
			target = "audio"
		}
		m.Routes = append(m.Routes, Route{
			SourcePID: s.PID, TrackID: id, Kind: s.Kind, Codec: s.Codec, Target: target,
		})
	}
	return m, m.StructuralValidate()
}

func invalid(msg string) *media.Error {
	return &media.Error{Code: media.CodeMappingInvalid, Component: "mapping", Message: msg}
}
