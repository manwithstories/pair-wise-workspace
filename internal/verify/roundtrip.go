// Package verify re-parses serialized output and diffs it cue by cue
// against the original unified timeline.
package verify

import (
	"math/big"
	"sort"

	"subtool/internal/model"
	"subtool/internal/parse"
	"subtool/internal/serialize"
)

// CueDrift is the per-cue comparison row.
type CueDrift struct {
	ID        string
	StartMs   float64
	EndMs     float64
	DriftMs   float64
	TextMatch bool
	Status    string // OK | DRIFT | DROPPED | TEXT
}

// Result aggregates one round-trip comparison.
type Result struct {
	Serialized   string
	Rows         []CueDrift
	MaxDriftMs   float64
	P99DriftMs   float64
	DroppedCues  int
	AddedCues    int
	TextMismatch int
}

// ParseByFormat dispatches to the parser for a format name.
func ParseByFormat(text, format string) *parse.Result {
	switch format {
	case serialize.SRT:
		return parse.ParseSRT(text)
	case serialize.VTT:
		return parse.ParseVTT(text)
	case serialize.ASS:
		return parse.ParseASS(text)
	case serialize.TTML:
		return parse.ParseTTML(text)
	}
	return &parse.Result{}
}

// Roundtrip serializes cues, re-parses the output with the same parse
// layer and diffs by cue ID. Duplicate IDs are matched in start order.
func Roundtrip(cues []model.Cue, format string) (*Result, error) {
	text, err := serialize.Write(cues, format)
	if err != nil {
		return nil, err
	}
	back := ParseByFormat(text, format)
	res := &Result{Serialized: text}

	byID := map[string][]int{}
	for i, c := range back.Cues {
		byID[c.ID] = append(byID[c.ID], i)
	}
	matched := make([]bool, len(back.Cues))
	used := map[string]int{}
	// match finds the re-parsed cue for orig: first by ID, then by
	// exact (start, end) equality to survive positional ID formats.
	match := func(orig model.Cue) (model.Cue, bool) {
		k := used[orig.ID]
		cands := byID[orig.ID]
		if k < len(cands) {
			used[orig.ID] = k + 1
			matched[cands[k]] = true
			return back.Cues[cands[k]], true
		}
		for i, c := range back.Cues {
			if matched[i] {
				continue
			}
			if c.Start.Cmp(orig.Start) == 0 && c.End.Cmp(orig.End) == 0 {
				matched[i] = true
				return c, true
			}
		}
		return model.Cue{}, false
	}
	var drifts []float64
	for _, orig := range cues {
		got, ok := match(orig)
		if !ok {
			res.DroppedCues++
			res.Rows = append(res.Rows, CueDrift{
				ID: orig.ID, Status: "DROPPED",
				StartMs: model.RatToFloat(orig.Start), EndMs: model.RatToFloat(orig.End),
			})
			continue
		}
		drift := absDiff(orig.Start, got.Start) + absDiff(orig.End, got.End)
		textOK := orig.Text == got.Text
		status := "OK"
		if !textOK {
			res.TextMismatch++
			status = "TEXT"
		}
		drifts = append(drifts, drift)
		if drift > res.MaxDriftMs {
			res.MaxDriftMs = drift
		}
		res.Rows = append(res.Rows, CueDrift{
			ID: orig.ID, StartMs: model.RatToFloat(orig.Start),
			EndMs: model.RatToFloat(orig.End), DriftMs: drift,
			TextMatch: textOK, Status: status,
		})
	}
	for i := range back.Cues {
		if !matched[i] {
			res.AddedCues++
		}
	}
	res.P99DriftMs = percentile(drifts, 0.99)
	sort.SliceStable(res.Rows, func(i, j int) bool { return res.Rows[i].ID < res.Rows[j].ID })
	return res, nil
}

func absDiff(a, b *big.Rat) float64 {
	d := new(big.Rat).Sub(a, b)
	d.Abs(d)
	v, _ := d.Float64()
	return v
}

func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	idx := int(p*float64(len(sorted)-1) + 0.5)
	return sorted[idx]
}
