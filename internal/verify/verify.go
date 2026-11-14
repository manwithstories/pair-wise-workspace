package verify

import (
	"bytes"
	"fmt"
	"math"
	"reflect"
	"sort"
	"sync"

	"segmanifest/internal/manifest"
)

type Result struct {
	ExpandedPaths   int      `json:"expanded_paths"`
	DuplicatePaths  int      `json:"duplicate_paths"`
	Holes           int      `json:"holes"`
	OverlapMS       float64  `json:"overlap_ms"`
	RoundtripOK     bool     `json:"roundtrip_ok"`
	Representations int      `json:"representations"`
	Segments        int      `json:"segments"`
	OK              bool     `json:"ok"`
	Problems        []string `json:"problems,omitempty"`
}

type repResult struct {
	paths      []string
	holes      int
	overlapUS  int64
	firstStart int64
	firstDur   int64
	lastDur    int64
}

func Verify(m *manifest.Manifest) Result {
	result := Result{}
	if err := m.Validate(); err != nil {
		result.Problems = append(result.Problems, err.Error())
		return result
	}
	result.Representations = len(m.Representations)
	for _, rep := range m.Representations {
		result.Segments += len(rep.Segments)
	}
	template, err := manifest.ParseTemplate(m.Template)
	if err != nil {
		result.Problems = append(result.Problems, err.Error())
		return result
	}
	results := make([]repResult, len(m.Representations))
	errs := make([]error, len(m.Representations))
	var wg sync.WaitGroup
	for i := range m.Representations {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			rep := m.Representations[index]
			paths := make([]string, 0, len(rep.Segments))
			for _, seg := range rep.Segments {
				path, err := template.Expand(rep.ID, rep.Bandwidth, seg.Number, seg.StartUS)
				if err != nil {
					errs[index] = err
					return
				}
				paths = append(paths, path)
			}
			results[index] = inspectRepresentation(rep, paths, m.Window.StartNumber)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			result.Problems = append(result.Problems, fmt.Sprintf("representation %s: %v", m.Representations[i].ID, err))
		}
	}
	if len(result.Problems) > 0 {
		return result
	}
	seen := make(map[string]struct{}, result.Segments)
	for i, rep := range results {
		result.Holes += rep.holes
		result.OverlapMS += float64(rep.overlapUS) / 1000
		result.ExpandedPaths += len(rep.paths)
		result.Problems = appendSegmentDurationProblems(result.Problems, m, i, rep)
		for _, path := range rep.paths {
			if _, ok := seen[path]; ok {
				result.DuplicatePaths++
			} else {
				seen[path] = struct{}{}
			}
		}
	}
	roundtripOK, err := roundTrip(m)
	result.RoundtripOK = roundtripOK
	if err != nil {
		result.Problems = append(result.Problems, "roundtrip: "+err.Error())
	}
	result.OverlapMS = math.Round(result.OverlapMS*1000) / 1000
	result.OK = result.DuplicatePaths == 0 && result.Holes == 0 && result.OverlapMS == 0 && result.RoundtripOK && len(result.Problems) == 0
	return result
}

func appendSegmentDurationProblems(problems []string, m *manifest.Manifest, index int, result repResult) []string {
	rep := m.Representations[index]
	if m.Window.StartNumber == 0 && result.firstStart != 0 {
		problems = append(problems, fmt.Sprintf("representation %s: first segment starts at %dus instead of zero", rep.ID, result.firstStart))
	}
	if difference := abs64(result.firstDur - m.TargetUS); difference > m.ToleranceUS {
		problems = append(problems, fmt.Sprintf("representation %s: first segment duration differs from target by %dus", rep.ID, difference))
	}
	if difference := abs64(result.lastDur - m.TargetUS); difference > m.ToleranceUS {
		problems = append(problems, fmt.Sprintf("representation %s: last segment duration differs from target by %dus", rep.ID, difference))
	}
	return problems
}

func abs64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func inspectRepresentation(rep manifest.Representation, paths []string, windowStart uint64) repResult {
	result := repResult{paths: paths}
	if len(rep.Segments) == 0 {
		return result
	}
	start := uint64(1)
	if windowStart > 0 {
		start = windowStart
	}
	first := rep.Segments[0]
	if first.Number > start {
		result.holes += int(first.Number - start)
	}
	result.firstStart = first.StartUS
	result.firstDur = first.DurationUS
	for i := 1; i < len(rep.Segments); i++ {
		previous, current := rep.Segments[i-1], rep.Segments[i]
		if current.Number > previous.Number+1 {
			result.holes += int(current.Number - previous.Number - 1)
		}
		previousEnd := previous.StartUS + previous.DurationUS
		if previousEnd > current.StartUS {
			result.overlapUS += previousEnd - current.StartUS
		}
	}
	last := rep.Segments[len(rep.Segments)-1]
	result.lastDur = last.DurationUS
	return result
}

func roundTrip(m *manifest.Manifest) (bool, error) {
	var buffer bytes.Buffer
	if err := manifest.Encode(m, &buffer); err != nil {
		return false, err
	}
	decoded, err := manifest.Decode(&buffer)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(m, decoded), nil
}

func SortProblems(result *Result) {
	sort.Strings(result.Problems)
}
