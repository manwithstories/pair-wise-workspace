package verify

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"dashpack/internal/manifest"
	"dashpack/internal/mp4"
)

const targetTimescale int64 = 90000

type Options struct {
	ManifestPath string
	Input        string
	ToleranceMS  int64
}

type Result struct {
	Representations int
	CheckedSegments int
	MaxDeviationMS  string
	MissingSegments int
	Gaps            int
	Overlaps        int
}

type repOutcome struct {
	id            string
	kind          string
	totalDuration int64
	gridTicks     int64
}

func Verify(options Options) (*Result, error) {
	if options.ManifestPath == "" || options.Input == "" {
		return nil, manifest.NewError("PARAM_INVALID",
			"mpd and input directories are required", 2, nil)
	}
	if options.ToleranceMS < 0 {
		return nil, manifest.NewError("PARAM_INVALID",
			"tolerance-ms must not be negative", 2, nil)
	}
	mpd, err := manifest.ParseFile(options.ManifestPath)
	if err != nil {
		return nil, manifest.NewError("MPD_PARSE",
			fmt.Sprintf("read manifest: %v", err), 4, nil)
	}
	if err := mpd.Validate(); err != nil {
		return nil, err
	}
	tolerance, err := scale(options.ToleranceMS*1000, 1000, targetTimescale)
	if err != nil {
		return nil, manifest.NewError("MPD_INVALID", err.Error(), 4, nil)
	}
	result := &Result{Representations: len(mpd.AllRepresentations())}
	maxDeviation := int64(0)
	var videoTotals, audioTotals []int64
	globalGrid := int64(0)
	for periodIndex := range mpd.Periods {
		period := &mpd.Periods[periodIndex]
		for setIndex := range period.AdaptationSets {
			set := &period.AdaptationSets[setIndex]
			kind := set.ContentType
			if kind == "" {
				if len(set.MimeType) >= 6 {
					kind = set.MimeType[:6]
					if kind == "video/" || kind == "audio/" {
						kind = kind[:5]
					}
				}
			}
			for reprIndex := range set.Representations {
				repr := &set.Representations[reprIndex]
				outcome, deviation, err := verifyRepresentation(options,
					repr, kind, tolerance, result)
				if err != nil {
					return nil, err
				}
				if deviation > maxDeviation {
					maxDeviation = deviation
				}
				if globalGrid == 0 {
					globalGrid = outcome.gridTicks
				}
				if kind == "video" {
					videoTotals = append(videoTotals, outcome.totalDuration)
				} else if kind == "audio" {
					audioTotals = append(audioTotals, outcome.totalDuration)
				}
			}
		}
	}
	result.MaxDeviationMS = formatMillis3(maxDeviation, targetTimescale)
	if result.MissingSegments > 0 {
		return result, verificationError("VERIFY_MISSING_SEGMENT",
			fmt.Sprintf("%d segment file(s) are missing", result.MissingSegments), result)
	}
	if result.Gaps > 0 {
		return result, verificationError("VERIFY_GAP",
			fmt.Sprintf("%d timeline gap(s) exceed tolerance", result.Gaps), result)
	}
	if result.Overlaps > 0 {
		return result, verificationError("VERIFY_OVERLAP",
			fmt.Sprintf("%d timeline overlap(s) exceed tolerance", result.Overlaps), result)
	}
	if maxDeviation > tolerance {
		return result, verificationError("VERIFY_DRIFT",
			fmt.Sprintf("maximum deviation %s ms exceeds tolerance",
				result.MaxDeviationMS), result)
	}
	if durationMismatch(videoTotals, audioTotals, globalGrid) {
		return result, verificationError("AV_DURATION_MISMATCH",
			"audio and video total durations differ by more than one segment", result)
	}
	return result, nil
}

func verifyRepresentation(options Options, repr *manifest.Representation, kind string,
	tolerance int64, result *Result) (repOutcome, int64, error) {
	template := repr.SegmentTemplate
	expanded, err := manifest.ExpandRepresentation(repr)
	if err != nil {
		return repOutcome{}, 0, manifest.NewError("MPD_INVALID",
			fmt.Sprintf("expand %q: %v", repr.ID, err), 4, repr.ID)
	}
	if template.Initialization != "" {
		initURL := manifest.ExpandTemplate(template.Initialization, manifest.ExpandContext{
			RepresentationID: repr.ID, Bandwidth: repr.Bandwidth,
		})
		initialization, err := manifest.SafeJoin(options.Input, initURL)
		if err != nil {
			return repOutcome{}, 0, manifest.NewError("PATH_ESCAPE", err.Error(), 4, repr.ID)
		}
		if _, err := os.Stat(initialization); err != nil {
			return repOutcome{}, 0, verificationError("MISSING_INIT",
				fmt.Sprintf("initialization for %q is missing", repr.ID), result)
		}
	}
	grid := int64(0)
	if len(template.Timeline.Segments) > 0 {
		grid, err = scale(template.Timeline.Segments[0].Duration,
			template.Timescale, targetTimescale)
		if err != nil {
			return repOutcome{}, 0, manifest.NewError("MPD_INVALID", err.Error(), 4, repr.ID)
		}
	}
	offset, err := scale(template.PresentationTimeOffset, template.Timescale, targetTimescale)
	if err != nil {
		return repOutcome{}, 0, manifest.NewError("MPD_INVALID", err.Error(), 4, repr.ID)
	}
	var previousStart, previousEnd int64
	havePrevious := false
	maxDeviation := int64(0)
	totalDuration := int64(0)
	for index, expandedSegment := range expanded {
		path, err := manifest.SafeJoin(options.Input, expandedSegment.URL)
		if err != nil {
			return repOutcome{}, 0, manifest.NewError("PATH_ESCAPE", err.Error(), 4, repr.ID)
		}
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				result.MissingSegments++
				continue
			}
			return repOutcome{}, 0, err
		}
		infos, err := mp4.ReadSegmentFile(path, uint32(template.Timescale))
		if err != nil {
			return repOutcome{}, 0, verificationError("VERIFY_PARSE",
				fmt.Sprintf("parse %s: %v", expandedSegment.URL, err), result)
		}
		if len(infos) == 0 {
			return repOutcome{}, 0, verificationError("VERIFY_PARSE",
				fmt.Sprintf("segment %s has no fragments", expandedSegment.URL), result)
		}
		for _, info := range infos {
			actualStart, err := scale(info.StartTime, int64(info.Timescale), targetTimescale)
			if err != nil {
				return repOutcome{}, 0, manifest.NewError("MPD_INVALID", err.Error(), 4, repr.ID)
			}
			actualDuration, err := scale(info.Duration, int64(info.Timescale), targetTimescale)
			if err != nil {
				return repOutcome{}, 0, manifest.NewError("MPD_INVALID", err.Error(), 4, repr.ID)
			}
			expected := offset + int64(index)*grid
			if delta := abs(actualStart - expected); delta > maxDeviation {
				maxDeviation = delta
			}
			if havePrevious {
				if actualStart < previousStart {
					return repOutcome{}, 0, verificationError("VERIFY_TIMELINE",
						fmt.Sprintf("representation %q is not monotonic", repr.ID), result)
				}
				boundary := actualStart - previousEnd
				if boundary > tolerance {
					result.Gaps++
				} else if boundary < -tolerance {
					result.Overlaps++
				}
			}
			previousStart, previousEnd = actualStart, actualStart+actualDuration
			havePrevious = true
			totalDuration += actualDuration
			result.CheckedSegments++
		}
	}
	return repOutcome{id: repr.ID, kind: kind, totalDuration: totalDuration,
		gridTicks: grid}, maxDeviation, nil
}

func durationMismatch(video, audio []int64, grid int64) bool {
	if len(video) == 0 || len(audio) == 0 || grid <= 0 {
		return false
	}
	minValue, maxValue := video[0], video[0]
	for _, value := range append(append([]int64{}, video...), audio...) {
		if value < minValue {
			minValue = value
		}
		if value > maxValue {
			maxValue = value
		}
	}
	return maxValue-minValue > grid
}

func verificationError(code, message string, result *Result) error {
	return manifest.NewError(code, message, 4, map[string]any{
		"representations":  result.Representations,
		"checked_segments": result.CheckedSegments,
		"max_deviation_ms": result.MaxDeviationMS,
		"missing_segments": result.MissingSegments,
		"gaps":             result.Gaps,
		"overlaps":         result.Overlaps,
	})
}

func scale(value, from, to int64) (int64, error) {
	if from <= 0 || to <= 0 {
		return 0, fmt.Errorf("invalid timebase")
	}
	g := gcd(from, to)
	from /= g
	to /= g
	if value != 0 && (value > int64(^uint64(0)>>1)/to ||
		value < -int64(^uint64(0)>>1)/to) {
		return 0, fmt.Errorf("timebase conversion overflow")
	}
	return (value * to) / from, nil
}

func gcd(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func formatMillis3(ticks, timescale int64) string {
	if ticks < 0 {
		ticks = -ticks
	}
	micros := ticks / timescale * 1_000_000
	if remainder := ticks % timescale; remainder != 0 {
		micros += remainder * 1_000_000 / timescale
	}
	return strconv.FormatInt(micros/1000, 10) + "." +
		fmt.Sprintf("%03d", micros%1000)
}
