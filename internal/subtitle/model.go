package subtitle

import (
	"fmt"
	"sort"
	"strings"
)

const MaxTimelineMS int64 = 99 * 60 * 60 * 1000

type Format string

const (
	FormatSRT Format = "srt"
	FormatASS Format = "ass"
	FormatVTT Format = "vtt"
)

type Cue struct {
	Index        int    `json:"index"`
	StartMS      int64  `json:"in_ms"`
	EndMS        int64  `json:"out_ms"`
	Text         string `json:"text"`
	ID           string `json:"id,omitempty"`
	Layer        string `json:"layer,omitempty"`
	Style        string `json:"style,omitempty"`
	Name         string `json:"name,omitempty"`
	MarginL      string `json:"margin_l,omitempty"`
	MarginR      string `json:"margin_r,omitempty"`
	MarginV      string `json:"margin_v,omitempty"`
	Effect       string `json:"effect,omitempty"`
	OverlapGroup int    `json:"overlap_group,omitempty"`
	Invalid      bool   `json:"invalid,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

type Track struct {
	Format Format `json:"format"`
	Cues   []Cue  `json:"cues"`
}

type ParseError struct {
	Code       string
	Message    string
	ParsedCues int
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("%s: %s (parsed_cues=%d)", e.Code, e.Message, e.ParsedCues)
}

func NewParseError(code, message string, parsed int) error {
	return &ParseError{Code: code, Message: message, ParsedCues: parsed}
}

func Normalize(t *Track) {
	if t == nil {
		return
	}
	sort.SliceStable(t.Cues, func(i, j int) bool {
		return t.Cues[i].StartMS < t.Cues[j].StartMS
	})
	for i := range t.Cues {
		c := &t.Cues[i]
		c.OverlapGroup = 0
		if c.Reason == "invalid_cue" {
			c.Reason = ""
		}
		c.Invalid = false
		if c.EndMS <= c.StartMS {
			c.Invalid = true
			c.Reason = "invalid_cue"
		}
	}
	assignOverlapGroups(t.Cues)
}

func assignOverlapGroups(cues []Cue) {
	group := 0
	for i := 0; i < len(cues); i++ {
		if cues[i].Invalid {
			continue
		}
		j := i + 1
		maxEnd := cues[i].EndMS
		for j < len(cues) && !cues[j].Invalid && cues[j].StartMS < maxEnd {
			if cues[j].EndMS > maxEnd {
				maxEnd = cues[j].EndMS
			}
			j++
		}
		if j-i >= 2 {
			group++
			for k := i; k < j; k++ {
				cues[k].OverlapGroup = group
			}
			i = j - 1
		}
	}
}

func OverlapGroupCount(cues []Cue) int {
	count := 0
	for i := range cues {
		if cues[i].OverlapGroup > count {
			count = cues[i].OverlapGroup
		}
	}
	return count
}

func DroppedCues(cues []Cue) []Cue {
	var out []Cue
	for _, c := range cues {
		if c.Invalid || c.Reason != "" {
			out = append(out, c)
		}
	}
	return out
}

func InvalidCount(cues []Cue) int {
	n := 0
	for _, c := range cues {
		if c.Invalid || c.Reason == "invalid_cue" {
			n++
		}
	}
	return n
}

func DetectFormat(name string, data []byte) Format {
	ext := strings.ToLower(name)
	if i := strings.LastIndexByte(ext, '.'); i >= 0 {
		ext = ext[i+1:]
	}
	switch ext {
	case "srt":
		return FormatSRT
	case "ass", "ssa":
		return FormatASS
	case "vtt":
		return FormatVTT
	}
	head := strings.TrimSpace(string(data))
	if strings.HasPrefix(head, "WEBVTT") {
		return FormatVTT
	}
	if strings.Contains(head[:min(len(head), 2048)], "[Events]") {
		return FormatASS
	}
	return FormatSRT
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
