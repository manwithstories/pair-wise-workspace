package manifest

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const StaticProfile = "urn:mpeg:dash:profile:isoff-live:2011"

type ExpandContext struct {
	RepresentationID string
	Bandwidth        int64
	Number           int64
	Time             int64
}

type ExpandedSegment struct {
	RepresentationID string
	PeriodID         string
	Number           int64
	Time             int64
	Duration         int64
	URL              string
}

func Marshal(m *MPD) ([]byte, error) {
	body, err := xml.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), append(body, '\n')...), nil
}

func Unmarshal(data []byte) (*MPD, error) {
	var m MPD
	if err := xml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func ParseFile(path string) (*MPD, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Unmarshal(data)
}

func ExpandTemplate(template string, ctx ExpandContext) string {
	replacer := strings.NewReplacer(
		"$RepresentationID$", ctx.RepresentationID,
		"$Bandwidth$", strconv.FormatInt(ctx.Bandwidth, 10),
		"$Number$", strconv.FormatInt(ctx.Number, 10),
		"$Time$", strconv.FormatInt(ctx.Time, 10),
	)
	return replacer.Replace(template)
}

func ExpandRepresentation(repr *Representation) ([]ExpandedSegment, error) {
	if repr == nil || repr.SegmentTemplate == nil {
		return nil, fmt.Errorf("representation has no segment template")
	}
	template := repr.SegmentTemplate
	if template.Timeline == nil {
		return nil, fmt.Errorf("representation %q has no timeline", repr.ID)
	}
	startNumber := template.StartNumber
	if startNumber == 0 {
		startNumber = 1
	}
	var result []ExpandedSegment
	current := int64(0)
	for _, segment := range template.Timeline.Segments {
		if segment.Time != nil {
			current = *segment.Time
		}
		if segment.Duration <= 0 {
			return nil, fmt.Errorf("representation %q has invalid duration", repr.ID)
		}
		if segment.Repeat < 0 || segment.Repeat > 1_000_000 {
			return nil, fmt.Errorf("representation %q has unsupported repeat", repr.ID)
		}
		for repeat := int64(0); repeat <= segment.Repeat; repeat++ {
			number := startNumber + int64(len(result))
			ctx := ExpandContext{
				RepresentationID: repr.ID,
				Bandwidth:        repr.Bandwidth,
				Number:           number,
				Time:             current,
			}
			result = append(result, ExpandedSegment{
				RepresentationID: repr.ID,
				Number:           number,
				Time:             current,
				Duration:         segment.Duration,
				URL:              ExpandTemplate(template.Media, ctx),
			})
			current += segment.Duration
		}
	}
	return result, nil
}

func ExpandMPD(m *MPD) ([]ExpandedSegment, error) {
	var result []ExpandedSegment
	for i := range m.Periods {
		for j := range m.Periods[i].AdaptationSets {
			for k := range m.Periods[i].AdaptationSets[j].Representations {
				segments, err := ExpandRepresentation(&m.Periods[i].AdaptationSets[j].Representations[k])
				if err != nil {
					return nil, err
				}
				for n := range segments {
					segments[n].PeriodID = m.Periods[i].ID
				}
				result = append(result, segments...)
			}
		}
	}
	return result, nil
}

func URLs(segments []ExpandedSegment) []string {
	result := make([]string, len(segments))
	for i := range segments {
		result[i] = segments[i].URL
	}
	return result
}

func HeadTail(segments []ExpandedSegment, count int) []string {
	if count < 0 {
		count = 0
	}
	if len(segments) <= count*2 {
		return URLs(segments)
	}
	result := append([]string(nil), URLs(segments[:count])...)
	return append(result, URLs(segments[len(segments)-count:])...)
}

// SafeJoin resolves a manifest URL beneath root and rejects traversal and
// symlink escapes. Missing files are still resolved through their real parent.
func SafeJoin(root, relative string) (string, error) {
	if root == "" || relative == "" {
		return "", fmt.Errorf("root and URL path are required")
	}
	if strings.ContainsRune(relative, 0) || strings.Contains(relative, "\\") ||
		strings.Contains(relative, "://") || filepath.IsAbs(relative) ||
		filepath.VolumeName(relative) != "" {
		return "", fmt.Errorf("unsafe URL path %q", relative)
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("URL path escapes input root: %q", relative)
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve input root: %w", err)
	}
	rootAbs, err := filepath.Abs(rootReal)
	if err != nil {
		return "", err
	}
	target := filepath.Join(rootAbs, clean)
	resolved, err := resolveMissingPath(target)
	if err != nil {
		return "", err
	}
	within, err := filepath.Rel(rootAbs, resolved)
	if err != nil {
		return "", err
	}
	if within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) ||
		filepath.IsAbs(within) {
		return "", fmt.Errorf("URL path resolves outside input root: %q", relative)
	}
	return resolved, nil
}

func resolveMissingPath(target string) (string, error) {
	probe := target
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
}
