package mp4

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MaxSegmentsPerRepresentation caps one representation at 4096 segments.
const MaxSegmentsPerRepresentation = 4096

// Segment is one entry of a representation timeline, in timescale units.
type Segment struct {
	Number   uint32 `json:"number"`   // mfhd sequence number
	Start    uint64 `json:"start"`    // decode start, timescale units
	Duration uint64 `json:"duration"` // trun accumulated duration
	HasTfdt  bool   `json:"has_tfdt"` // false when start was inferred by accumulation
	Inferred bool   `json:"inferred"` // true for gap-fill entries (no file on disk)
	File     string `json:"file,omitempty"`
}

// Timeline is the scanned view of one representation directory.
type Timeline struct {
	Dir          string    `json:"dir"`
	Timescale    uint32    `json:"timescale"`
	InitDuration uint64    `json:"init_duration"`
	InitFile     string    `json:"init_file"`
	TrexDefault  uint32    `json:"trex_default_sample_duration"`
	Segments     []Segment `json:"segments"`
	Gaps         int       `json:"gaps"`
	Overlaps     int       `json:"overlaps"`
	NonMonotonic int       `json:"non_monotonic"`
	ShortTail    bool      `json:"short_tail"`
}

// ScanRepresentation scans one representation directory: exactly one init
// segment (*.mp4) plus media segments (*.m4s). Segment start is the tfdt
// base value; when tfdt is absent the start is inferred by accumulation.
func ScanRepresentation(dir string) (*Timeline, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", dir, err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("scan %s: empty representation directory", dir)
	}
	var initFile string
	var segFiles []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".mp4"):
			if initFile != "" {
				return nil, fmt.Errorf("scan %s: multiple init segments (%s, %s)", dir, initFile, name)
			}
			initFile = name
		case strings.HasSuffix(name, ".m4s"):
			segFiles = append(segFiles, name)
		}
	}
	if initFile == "" {
		return nil, fmt.Errorf("scan %s: missing init segment (*.mp4)", dir)
	}
	if len(segFiles) == 0 {
		return nil, fmt.Errorf("scan %s: no media segments (*.m4s)", dir)
	}
	if len(segFiles) > MaxSegmentsPerRepresentation {
		return nil, fmt.Errorf("scan %s: %d segments exceeds limit %d", dir, len(segFiles), MaxSegmentsPerRepresentation)
	}
	initData, err := os.ReadFile(filepath.Join(dir, initFile))
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", dir, err)
	}
	initInfo, err := ParseInit(initData)
	if err != nil {
		return nil, fmt.Errorf("scan %s: %s: %v", dir, initFile, err)
	}
	tl := &Timeline{
		Dir:          dir,
		Timescale:    initInfo.Timescale,
		InitDuration: initInfo.Duration,
		InitFile:     initFile,
		TrexDefault:  initInfo.TrexDefaultSampleDuration,
	}
	sort.Strings(segFiles)
	parsed := make([]Segment, 0, len(segFiles))
	for _, name := range segFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", dir, err)
		}
		si, err := ParseSegment(data)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %s: %v", dir, name, err)
		}
		if !si.HasSequence {
			return nil, fmt.Errorf("scan %s: %s: missing mfhd sequence number", dir, name)
		}
		dur, err := resolveDuration(si, initInfo)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %s: %v", dir, name, err)
		}
		parsed = append(parsed, Segment{
			Number:   si.SequenceNumber,
			Start:    si.BaseDecodeTime,
			Duration: dur,
			HasTfdt:  si.HasTfdt,
			File:     name,
		})
	}
	// Order the timeline by sequence number (mfhd), then resolve starts.
	sort.SliceStable(parsed, func(i, j int) bool { return parsed[i].Number < parsed[j].Number })
	var prevEnd, prevStart uint64
	for i := range parsed {
		seg := &parsed[i]
		if !seg.HasTfdt {
			seg.Start = prevEnd // accumulate when tfdt is absent
		}
		if i > 0 {
			switch {
			case seg.Start > prevEnd:
				tl.Gaps++
			case seg.Start < prevEnd:
				tl.Overlaps++
			}
			if seg.Start < prevStart {
				tl.NonMonotonic++ // decode time moved backwards in sequence order
			}
		}
		prevStart = seg.Start
		prevEnd = seg.Start + seg.Duration
		tl.Segments = append(tl.Segments, *seg)
	}
	if len(tl.Segments) > 1 {
		median := tl.NominalDuration()
		last := tl.Segments[len(tl.Segments)-1]
		if median > 0 && last.Duration*4 < median*3 {
			tl.ShortTail = true
		}
	}
	return tl, nil
}

// resolveDuration picks trun accumulated duration, then tfhd default, then
// trex default, each multiplied by the sample count as needed.
func resolveDuration(si *SegmentInfo, init *InitInfo) (uint64, error) {
	if si.HasTrunDuration {
		return si.TrunDuration, nil
	}
	if si.HasTfhdDefault {
		return uint64(si.SampleCount) * uint64(si.TfhdDefaultSample), nil
	}
	if init.TrexDefaultSampleDuration > 0 {
		return uint64(si.SampleCount) * uint64(init.TrexDefaultSampleDuration), nil
	}
	return 0, fmt.Errorf("cannot resolve segment duration (no trun durations, no tfhd/trex default)")
}

// NominalDuration returns the median segment duration in timescale units.
func (t *Timeline) NominalDuration() uint64 {
	if len(t.Segments) == 0 {
		return 0
	}
	durs := make([]uint64, len(t.Segments))
	for i, s := range t.Segments {
		durs[i] = s.Duration
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	return durs[len(durs)/2]
}

// End returns the end of the last segment in timescale units.
func (t *Timeline) End() uint64 {
	if len(t.Segments) == 0 {
		return 0
	}
	last := t.Segments[len(t.Segments)-1]
	return last.Start + last.Duration
}

// FilledSegments returns the real segments plus inferred gap-fill entries,
// so template expansion covers the full expected grid. Inferred entries
// inherit the nominal duration and the missing sequence numbers.
func (t *Timeline) FilledSegments() []Segment {
	if len(t.Segments) == 0 {
		return nil
	}
	nominal := t.NominalDuration()
	out := make([]Segment, 0, len(t.Segments))
	for i, seg := range t.Segments {
		if i > 0 {
			prev := t.Segments[i-1]
			prevEnd := prev.Start + prev.Duration
			if seg.Start > prevEnd && nominal > 0 {
				gap := seg.Start - prevEnd
				count := int((gap + nominal/2) / nominal)
				for k := 0; k < count; k++ {
					out = append(out, Segment{
						Number:   prev.Number + uint32(k) + 1,
						Start:    prevEnd + uint64(k)*nominal,
						Duration: nominal,
						Inferred: true,
					})
				}
			}
		}
		out = append(out, seg)
	}
	return out
}
