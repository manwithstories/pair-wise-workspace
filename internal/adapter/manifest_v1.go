package adapter

import (
	"bufio"
	"encoding/json"
	"fmt"

	"aircheck/internal/media"
)

type trackV1 struct {
	ID       string  `json:"id"`
	Priority int     `json:"priority"`
	FPS      float64 `json:"fps"`
	Modulus  int64   `json:"modulus"`
}

type headerV1 struct {
	Type     string `json:"type"`
	Version  int    `json:"version"`
	Timebase struct {
		Num int64 `json:"num"`
		Den int64 `json:"den"`
	} `json:"timebase"`
	Tracks []trackV1 `json:"tracks"`
}

type recordV1 struct {
	Type   string  `json:"type"`
	Track  string  `json:"track"`
	Seq    int64   `json:"seq"`
	PTS    int64   `json:"pts"`
	Dur    int64   `json:"dur"`
	SHA256 string  `json:"sha256"`
	File   string  `json:"file"`
	RefMs  float64 `json:"ref_ms"`
}

func init() { register(1, parseV1) }

func parseV1(first []byte, sc *bufio.Scanner, path string) (*Manifest, error) {
	var h headerV1
	if err := json.Unmarshal(first, &h); err != nil {
		return nil, fmt.Errorf("%s:1: invalid v1 header: %v", path, err)
	}
	m := &Manifest{Version: 1, Timebase: media.Timebase{Num: h.Timebase.Num, Den: h.Timebase.Den}}
	if err := m.Timebase.Validate(); err != nil {
		return nil, fmt.Errorf("%s:1: %v", path, err)
	}
	known := map[string]bool{}
	for _, t := range h.Tracks {
		if t.ID == "" || t.FPS <= 0 || t.Modulus < 0 {
			return nil, fmt.Errorf("%s:1: invalid track %+v", path, t)
		}
		if known[t.ID] {
			return nil, fmt.Errorf("%s:1: duplicate track %q", path, t.ID)
		}
		known[t.ID] = true
		m.Tracks = append(m.Tracks, Track{ID: t.ID, Priority: t.Priority, FPS: t.FPS, Modulus: t.Modulus})
	}
	if len(m.Tracks) == 0 {
		return nil, fmt.Errorf("%s:1: no tracks declared", path)
	}
	line := 1
	for sc.Scan() {
		line++
		var r recordV1
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("%s:%d: invalid record: %v", path, line, err)
		}
		switch r.Type {
		case "frame":
			if !known[r.Track] {
				return nil, fmt.Errorf("%s:%d: unknown track %q", path, line, r.Track)
			}
			if len(r.SHA256) != 64 {
				return nil, fmt.Errorf("%s:%d: bad sha256 for %s seq %d", path, line, r.Track, r.Seq)
			}
			if _, err := SecureJoin("x", r.File); err != nil {
				return nil, fmt.Errorf("%s:%d: %v", path, line, err)
			}
			m.Frames = append(m.Frames, FrameRec{Track: r.Track, Seq: r.Seq, PTS: r.PTS,
				Dur: r.Dur, SHA256: r.SHA256, File: r.File})
		case "anchor":
			if !known[r.Track] {
				return nil, fmt.Errorf("%s:%d: unknown track %q", path, line, r.Track)
			}
			m.Anchors = append(m.Anchors, AnchorRec{Track: r.Track, PTS: r.PTS, RefMs: r.RefMs})
		default:
			return nil, fmt.Errorf("%s:%d: unknown record type %q", path, line, r.Type)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: read error near line %d: %v", path, line, err)
	}
	if len(m.Frames) == 0 {
		return nil, fmt.Errorf("%s: no frame records", path)
	}
	return m, nil
}
