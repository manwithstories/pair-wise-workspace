// Package manifest reads and validates shot manifests (素材清单):
// sparse keyframes, edit lists and non-zero start timestamps, all on the
// shared rational clock. Invalid entries are rejected with their exact
// input location.
package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"

	"fieldqc/internal/clock"
	"fieldqc/internal/fault"
)

// Edit is one edit-list entry, in ticks of the asset's first track.
type Edit struct {
	MediaTime int64 `json:"media_time"`
	Duration  int64 `json:"duration"`
	Rate      int   `json:"rate"`
}

// ColorMeta carries color metadata declared by a track.
type ColorMeta struct {
	Primaries string `json:"primaries"`
	Transfer  string `json:"transfer"`
	Matrix    string `json:"matrix"`
	FullRange bool   `json:"full_range"`
}

// Track describes one essence track of an asset.
type Track struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"` // "video" or "audio"
	Timescale  int64      `json:"timescale"`
	Duration   int64      `json:"duration"`  // ticks
	StartDTS   int64      `json:"start_dts"` // non-zero start allowed
	Channels   int        `json:"channels,omitempty"`
	SampleRate int        `json:"sample_rate,omitempty"`
	Color      *ColorMeta `json:"color,omitempty"`
}

// Start returns the track's (possibly non-zero) start time in seconds.
func (t *Track) Start() (clock.Ratio, error) { return clock.Ticks(t.StartDTS, t.Timescale) }

// Asset is one camera card / source file.
type Asset struct {
	ID        string  `json:"id"`
	Edits     []Edit  `json:"edits"`
	Keyframes []int64 `json:"keyframes"` // sparse keyframe ticks
	Tracks    []Track `json:"tracks"`
}

// Duration returns the asset duration (max track duration) in seconds.
func (a *Asset) Duration() (clock.Ratio, error) {
	max := clock.Zero()
	for i := range a.Tracks {
		d, err := clock.Ticks(a.Tracks[i].Duration, a.Tracks[i].Timescale)
		if err != nil {
			return clock.Ratio{}, err
		}
		max = clock.Max(max, d)
	}
	return max, nil
}

// Manifest is the decoded shot list.
type Manifest struct {
	Assets []Asset `json:"assets"`
}

// Error locates one invalid input precisely.
type Error struct {
	Asset string
	Field string
	Code  fault.Code
	Msg   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("manifest: asset %q %s: %s (%s)", e.Asset, e.Field, e.Msg, e.Code)
}

// Errors aggregates validation failures.
type Errors []*Error

func (es Errors) Error() string {
	if len(es) == 0 {
		return "manifest: no errors"
	}
	if len(es) == 1 {
		return es[0].Error()
	}
	return fmt.Sprintf("%s (+%d more)", es[0].Error(), len(es)-1)
}

// Parse decodes and validates a manifest. Every invalid entry is
// rejected with its input location; overflow, negative durations and
// out-of-range edits are all faults.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: invalid JSON: %w", err)
	}
	var errs Errors
	for ai := range m.Assets {
		a := &m.Assets[ai]
		id := a.ID
		if id == "" {
			id = fmt.Sprintf("#%d", ai)
			errs = append(errs, &Error{id, "id", fault.CodeInvalidEdit, "empty asset id"})
		}
		assetDur, _ := a.Duration()
		var editScale int64
		if len(a.Tracks) > 0 {
			editScale = a.Tracks[0].Timescale
		}
		for ti := range a.Tracks {
			t := &a.Tracks[ti]
			field := fmt.Sprintf("tracks[%d]", ti)
			if t.Timescale <= 0 {
				errs = append(errs, &Error{id, field + ".timescale", fault.CodeOverflow, "timescale must be > 0"})
				continue
			}
			if t.Duration < 0 {
				errs = append(errs, &Error{id, field + ".duration", fault.CodeNegativeDuration, "negative duration"})
			}
			if t.Kind != "video" && t.Kind != "audio" {
				errs = append(errs, &Error{id, field + ".kind", fault.CodeInvalidEdit, "kind must be video or audio"})
			}
			if _, err := t.Start(); err != nil {
				errs = append(errs, &Error{id, field + ".start_dts", fault.CodeOverflow, err.Error()})
			}
		}
		for ei := range a.Edits {
			e := &a.Edits[ei]
			field := fmt.Sprintf("edits[%d]", ei)
			switch {
			case e.MediaTime < 0:
				errs = append(errs, &Error{id, field + ".media_time", fault.CodeInvalidEdit, "negative media_time"})
			case e.Duration <= 0:
				errs = append(errs, &Error{id, field + ".duration", fault.CodeNegativeDuration, "edit duration must be > 0"})
			case e.Rate <= 0:
				errs = append(errs, &Error{id, field + ".rate", fault.CodeInvalidEdit, "edit rate must be > 0"})
			case e.MediaTime > math.MaxInt64-e.Duration:
				errs = append(errs, &Error{id, field, fault.CodeOverflow, "media_time+duration overflows int64"})
			case editScale > 0:
				end, err := clock.Ticks(e.MediaTime+e.Duration, editScale)
				if err != nil {
					errs = append(errs, &Error{id, field, fault.CodeOverflow, err.Error()})
				} else if end.Cmp(assetDur) > 0 {
					errs = append(errs, &Error{id, field, fault.CodeEditRange,
						fmt.Sprintf("edit ends at %s beyond asset duration %s", end, assetDur)})
				}
			}
		}
		for ki, k := range a.Keyframes {
			field := fmt.Sprintf("keyframes[%d]", ki)
			if k < 0 {
				errs = append(errs, &Error{id, field, fault.CodeInvalidEdit, "negative keyframe tick"})
			}
			if ki > 0 && k < a.Keyframes[ki-1] {
				errs = append(errs, &Error{id, field, fault.CodeInvalidEdit, "keyframes not sorted"})
			}
			if editScale > 0 {
				if kt, err := clock.Ticks(k, editScale); err == nil && kt.Cmp(assetDur) > 0 {
					errs = append(errs, &Error{id, field, fault.CodeEditRange, "keyframe beyond asset duration"})
				}
			}
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return &m, nil
}
