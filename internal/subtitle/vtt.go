package subtitle

import "strings"

// ParseVTT parses a WebVTT document. Same contract as ParseSRT: on
// error the cues parsed so far are returned with a *ParseError.
func ParseVTT(src []byte) ([]Cue, error) {
	ls, offs := splitLines(src)
	if len(ls) == 0 || !strings.HasPrefix(ls[0], "WEBVTT") {
		return nil, &ParseError{Offset: 0, Kind: "timestamp", Msg: "missing WEBVTT header"}
	}
	i := 1
	var cues []Cue
	for {
		for i < len(ls) && strings.TrimSpace(ls[i]) == "" {
			i++
		}
		if i >= len(ls) {
			return cues, nil
		}
		cueOff := offs[i]
		// A timing line contains " --> "; anything before it is an
		// optional cue identifier.
		if !strings.Contains(ls[i], " --> ") {
			i++
			if i >= len(ls) {
				return cues, &ParseError{Offset: cueOff, Kind: "timestamp", Msg: "missing timing line"}
			}
		}
		start, end, err := parseTiming(ls[i], offs[i], '.', true)
		if err != nil {
			return cues, err
		}
		i++
		var text []string
		for i < len(ls) && strings.TrimSpace(ls[i]) != "" {
			text = append(text, ls[i])
			i++
		}
		cue := Cue{Index: len(cues) + 1, Start: start, End: end, Text: strings.Join(text, "\n"), Offset: cueOff}
		if err := checkOrder(cue, cues); err != nil {
			return cues, err
		}
		cues = append(cues, cue)
	}
}
