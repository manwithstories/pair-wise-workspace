package manifest

import (
	"fmt"
	"sort"
	"strings"
)

// MetadataHint is one metadata source (container box, sidecar JSON,
// manifest attribute, ...). Sources are reconciled without dropping
// values: disagreements are recorded on TrackDesc.MetadataConflict.
type MetadataHint struct {
	Source        string `json:"source"`
	Channels      int    `json:"channels,omitempty"`
	ChannelLayout string `json:"channel_layout,omitempty"`
	SampleRate    uint32 `json:"sample_rate,omitempty"`
	ColorInfo     string `json:"color_info,omitempty"`
}

// layoutChannels maps canonical layout labels to channel counts.
var layoutChannels = map[string]int{
	"mono": 1, "1.0": 1,
	"stereo": 2, "2.0": 2,
	"2.1": 3, "3.0": 3,
	"4.0": 4, "5.0": 5,
	"5.1": 6, "6.0": 6,
	"6.1": 7, "7.0": 7,
	"7.1": 8,
}

// uniqueLayoutForChannels only contains counts for which a single
// standard layout exists. Six channels, for example, may be 5.1 or 6.0
// and must never be guessed.
var uniqueLayoutForChannels = map[int]string{
	1: "mono",
	2: "stereo",
}

// ReconcileMetadata merges hints into a track description. It never
// fabricates metadata: channel layout is inferred only when the channel
// count identifies it uniquely; every disagreement or ambiguous
// inference is joined into MetadataConflict.
func ReconcileMetadata(t TrackDesc, hints ...MetadataHint) TrackDesc {
	channels := t.Channels
	layout := normalizeLayout(t.ChannelLayout)
	rate := t.SampleRate
	color := t.ColorInfo
	var conflicts []string

	addConflict := func(format string, args ...any) {
		conflicts = append(conflicts, fmt.Sprintf(format, args...))
	}

	for _, h := range hints {
		src := h.Source
		if src == "" {
			src = "hint"
		}
		if h.Channels > 0 {
			if channels != 0 && h.Channels != channels {
				addConflict("channels conflict: %d (%s) vs %d (earlier source)", h.Channels, src, channels)
			}
			if channels == 0 {
				channels = h.Channels
			}
		}
		if l := normalizeLayout(h.ChannelLayout); l != "" {
			if layout != "" && layout != l {
				addConflict("channel_layout conflict: %q (%s) vs %q (earlier source)", l, src, layout)
			}
			if layout == "" {
				layout = l
			}
		}
		if h.SampleRate != 0 {
			if rate != 0 && h.SampleRate != rate {
				addConflict("sample_rate conflict: %d (%s) vs %d (earlier source)", h.SampleRate, src, rate)
			}
			if rate == 0 {
				rate = h.SampleRate
			}
		}
		if h.ColorInfo != "" {
			c := normalizeColor(h.ColorInfo)
			if color != "" && normalizeColor(color) != c {
				addConflict("color_info conflict: %q (%s) vs %q (earlier source)", c, src, normalizeColor(color))
			}
			if color == "" {
				color = h.ColorInfo
			}
		}
	}

	// Cross-check explicit layout against explicit channel count.
	if channels > 0 && layout != "" {
		if want, ok := layoutChannels[layout]; ok && want != channels {
			addConflict("layout %q requires %d channels but stream declares %d", layout, want, channels)
		}
	}

	// Unique inference only; otherwise flag ambiguity instead of
	// inventing a layout.
	if layout == "" && channels > 0 {
		if inferred, ok := uniqueLayoutForChannels[channels]; ok {
			layout = inferred
		} else {
			addConflict("channel_layout not uniquely inferable from %d channels", channels)
		}
	}

	sort.Strings(conflicts)
	t.Channels = channels
	t.ChannelLayout = layout
	t.SampleRate = rate
	t.ColorInfo = color
	t.MetadataConflict = strings.Join(conflicts, "; ")
	return t
}

// HasMetadataConflict reports whether the track carries a conflict.
func (t TrackDesc) HasMetadataConflict() bool { return t.MetadataConflict != "" }

func normalizeLayout(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "")
	return s
}

func normalizeColor(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
