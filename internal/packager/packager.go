package packager

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"dashpack/internal/manifest"
	"dashpack/internal/mp4"
	"dashpack/internal/timeline"
)

type Options struct {
	Input       string
	Out         string
	SegmentMS   int64
	ToleranceMS int64
}

type Result struct {
	Representations  int
	Periods          int
	Segments         int
	ExpandedExamples []string
	MaxDeviationMS   string
	Gaps             int
	Overlaps         int
	Manifest         *manifest.MPD
	Expanded         []manifest.ExpandedSegment
	Alignment        *timeline.Report
}

type representationInput struct {
	ID        string
	Kind      string
	Timescale int64
	Width     uint32
	Height    uint32
	Bandwidth int64
	Segments  []timeline.Segment
}

type segmentFile struct {
	name    string
	order   int64
	numeric bool
}

func Pack(options Options) (*Result, error) {
	if options.Input == "" || options.Out == "" {
		return nil, manifest.NewError("PARAM_INVALID",
			"input and output directories are required", 2, nil)
	}
	if options.SegmentMS <= 0 || options.ToleranceMS < 0 {
		return nil, manifest.NewError("PARAM_INVALID",
			"segment-ms must be positive and tolerance-ms non-negative", 2, nil)
	}
	inAbs, err := filepath.Abs(options.Input)
	if err != nil {
		return nil, err
	}
	outAbs, err := filepath.Abs(options.Out)
	if err != nil {
		return nil, err
	}
	if inAbs == outAbs {
		return nil, manifest.NewError("PARAM_INVALID",
			"input and output directories must differ", 2, nil)
	}
	reps, err := discover(options.Input)
	if err != nil {
		return nil, err
	}
	if len(reps) == 0 {
		return nil, manifest.NewError("INPUT_STRUCTURE",
			"no representations with init.mp4 and segments were found", 2, nil)
	}
	sort.SliceStable(reps, func(i, j int) bool {
		if reps[i].Kind != reps[j].Kind {
			return reps[i].Kind == "video"
		}
		if reps[i].Kind == "video" && reps[i].Width != reps[j].Width {
			return reps[i].Width > reps[j].Width
		}
		return reps[i].ID < reps[j].ID
	})
	tracks := make([]timeline.Track, 0, len(reps))
	segmentCount := 0
	for repIndex := range reps {
		rep := &reps[repIndex]
		segments, err := loadSegments(options.Input, *rep)
		if err != nil {
			return nil, err
		}
		rep.Segments = segments
		segmentCount += len(segments)
		tracks = append(tracks, timeline.Track{
			ID: rep.ID, Kind: rep.Kind, Timescale: rep.Timescale, Segments: segments,
		})
	}
	alignment, err := timeline.Align(tracks, timeline.Config{
		TargetTimescale: 90000,
		GridMillis:      options.SegmentMS,
		ToleranceMillis: options.ToleranceMS,
	})
	if err != nil {
		return nil, manifest.NewError("INPUT_STRUCTURE", err.Error(), 2, nil)
	}
	if err := alignmentError(alignment); err != nil {
		return nil, err
	}
	mpd, expanded, err := buildManifest(reps, alignment, options.SegmentMS)
	if err != nil {
		return nil, err
	}
	result := &Result{
		Representations: len(reps), Periods: len(mpd.Periods), Segments: segmentCount,
		ExpandedExamples: firstURLs(expanded, 3),
		MaxDeviationMS: timeline.FormatMillis3(alignment.MaxDeviationTicks,
			alignment.TargetTimescale),
		Gaps: alignment.Gaps, Overlaps: alignment.Overlaps,
		Manifest: mpd, Expanded: expanded, Alignment: alignment,
	}
	if err := writeOutputs(options.Out, result); err != nil {
		return nil, err
	}
	return result, nil
}

func firstURLs(segments []manifest.ExpandedSegment, count int) []string {
	if count > len(segments) {
		count = len(segments)
	}
	return manifest.URLs(segments[:count])
}

func buildManifest(reps []representationInput, alignment *timeline.Report,
	segmentMS int64) (*manifest.MPD, []manifest.ExpandedSegment, error) {
	gridTicks, err := timeline.MillisToTicks(segmentMS, 90000)
	if err != nil {
		return nil, nil, err
	}
	videoSet := manifest.AdaptationSet{
		ID: "video", ContentType: "video", MimeType: "video/mp4",
	}
	audioSet := manifest.AdaptationSet{
		ID: "audio", ContentType: "audio", MimeType: "audio/mp4", Lang: "und",
	}
	maxEnd := int64(0)
	segmentCount := 0
	for _, rep := range reps {
		template := &manifest.SegmentTemplate{
			Timescale:              90000,
			PresentationTimeOffset: alignment.PresentationOffsets[rep.ID],
			StartNumber:            1,
			Media:                  "$RepresentationID$/seg-$Time$.m4s",
			Initialization:         "$RepresentationID$/init.mp4",
			Timeline:               &manifest.SegmentTimeline{},
		}
		for index, segment := range rep.Segments {
			start := mediaTime(segment, int64(index)*gridTicks, rep.Timescale, 90000)
			duration, err := timeline.ScaleTicks(segment.Duration, rep.Timescale, 90000)
			if err != nil {
				return nil, nil, err
			}
			timeValue := start
			template.Timeline.Segments = append(template.Timeline.Segments,
				manifest.S{Time: &timeValue, Duration: duration})
			normalizedEnd := start + duration - template.PresentationTimeOffset
			if normalizedEnd > maxEnd {
				maxEnd = normalizedEnd
			}
			segmentCount++
		}
		repr := manifest.Representation{
			ID: rep.ID, Bandwidth: rep.Bandwidth, SegmentTemplate: template,
			Width: int64(rep.Width), Height: int64(rep.Height),
		}
		if rep.Kind == "video" {
			repr.Codecs = "avc1.4d401f"
			videoSet.Representations = append(videoSet.Representations, repr)
		} else {
			repr.Codecs = "mp4a.40.2"
			repr.AudioSamplingRate = 48000
			audioSet.Representations = append(audioSet.Representations, repr)
		}
	}
	if maxEnd < 0 {
		maxEnd = 0
	}
	mpd := &manifest.MPD{
		XMLNS:                     "urn:mpeg:dash:schema:mpd:2011",
		Type:                      "static",
		Profiles:                  manifest.StaticProfile,
		MediaPresentationDuration: timeline.FormatISO8601(maxEnd, 90000),
		MinBufferTime:             timeline.FormatISO8601(gridTicks, 90000),
		Periods: []manifest.Period{{
			ID: "p0", Start: "PT0.000S",
			AdaptationSets: []manifest.AdaptationSet{videoSet, audioSet},
		}},
	}
	if len(audioSet.Representations) == 0 {
		mpd.Periods[0].AdaptationSets = []manifest.AdaptationSet{videoSet}
	}
	if len(videoSet.Representations) == 0 {
		mpd.Periods[0].AdaptationSets = []manifest.AdaptationSet{audioSet}
	}
	if err := mpd.Validate(); err != nil {
		return nil, nil, err
	}
	expanded, err := manifest.ExpandMPD(mpd)
	if err != nil {
		return nil, nil, err
	}
	if len(expanded) != segmentCount {
		return nil, nil, fmt.Errorf("expanded %d segments, loaded %d", len(expanded), segmentCount)
	}
	return mpd, expanded, nil
}

func mediaTime(segment timeline.Segment, fallback, timescale, target int64) int64 {
	name := filepath.Base(segment.Path)
	stem := strings.TrimSuffix(name, ".m4s")
	if strings.HasPrefix(stem, "seg-") {
		if value, err := strconv.ParseInt(strings.TrimPrefix(stem, "seg-"), 10, 64); err == nil {
			if converted, err := timeline.ScaleTicks(value, timescale, target); err == nil {
				return converted
			}
		}
	}
	return fallback
}

type expandedEntry struct {
	RepresentationID string `json:"representation"`
	Number           int64  `json:"number"`
	Time             int64  `json:"time"`
	Duration         int64  `json:"duration"`
	URL              string `json:"url"`
}

type expandedFile struct {
	Representations  int             `json:"representations"`
	Segments         int             `json:"segments"`
	ExpandedExamples []string        `json:"expanded_examples"`
	Entries          []expandedEntry `json:"entries"`
}

type alignmentFile struct {
	OK                  bool             `json:"ok"`
	Representations     int              `json:"representations"`
	Segments            int              `json:"segments"`
	MaxDeviationMS      string           `json:"max_deviation_ms"`
	Gaps                int              `json:"gaps"`
	Overlaps            int              `json:"overlaps"`
	PresentationOffsets map[string]int64 `json:"presentation_time_offsets"`
}

func writeOutputs(out string, result *Result) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	xmlData, err := manifest.Marshal(result.Manifest)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "manifest.mpd"), xmlData, 0o644); err != nil {
		return err
	}
	expanded := expandedFile{
		Representations: result.Representations, Segments: result.Segments,
		ExpandedExamples: result.ExpandedExamples,
	}
	for _, segment := range result.Expanded {
		expanded.Entries = append(expanded.Entries, expandedEntry{
			RepresentationID: segment.RepresentationID, Number: segment.Number,
			Time: segment.Time, Duration: segment.Duration, URL: segment.URL,
		})
	}
	if err := writeJSON(filepath.Join(out, "expanded.json"), expanded); err != nil {
		return err
	}
	alignment := alignmentFile{
		OK: true, Representations: result.Representations, Segments: result.Segments,
		MaxDeviationMS: result.MaxDeviationMS, Gaps: result.Gaps, Overlaps: result.Overlaps,
		PresentationOffsets: result.Alignment.PresentationOffsets,
	}
	return writeJSON(filepath.Join(out, "alignment.json"), alignment)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func discover(root string) ([]representationInput, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read input root: %w", err)
	}
	seen := make(map[string]bool)
	var result []representationInput
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read representation %q: %w", entry.Name(), err)
		}
		hasInit := false
		var segments []segmentFile
		for _, file := range files {
			if file.IsDir() {
				continue
			}
			switch {
			case file.Name() == "init.mp4":
				hasInit = true
			case strings.HasSuffix(file.Name(), ".m4s"):
				segments = append(segments, parseSegmentName(file.Name()))
			}
		}
		if !hasInit {
			if len(segments) > 0 {
				return nil, manifest.NewError("MISSING_INIT",
					fmt.Sprintf("representation %q has segments but no init.mp4", entry.Name()),
					2, entry.Name())
			}
			continue
		}
		if seen[entry.Name()] {
			return nil, manifest.NewError("DUPLICATE_REPRESENTATION",
				fmt.Sprintf("duplicate representation id %q", entry.Name()), 2, entry.Name())
		}
		seen[entry.Name()] = true
		if len(segments) == 0 {
			return nil, manifest.NewError("INPUT_STRUCTURE",
				fmt.Sprintf("representation %q has no segments", entry.Name()), 2, entry.Name())
		}
		initPath, err := manifest.SafeJoin(root, entry.Name()+"/init.mp4")
		if err != nil {
			return nil, manifest.NewError("PATH_ESCAPE", err.Error(), 2, entry.Name())
		}
		info, err := mp4.ParseInitFile(initPath)
		if err != nil {
			var parseErr *mp4.ParseError
			if errors.As(err, &parseErr) {
				return nil, manifest.NewError("INPUT_STRUCTURE",
					fmt.Sprintf("parse %s: %v", entry.Name(), err), 2, entry.Name())
			}
			return nil, err
		}
		kind := ""
		switch info.Handler {
		case "vide":
			kind = "video"
		case "soun":
			kind = "audio"
		default:
			return nil, manifest.NewError("INPUT_STRUCTURE",
				fmt.Sprintf("representation %q has unsupported handler %q", entry.Name(), info.Handler),
				2, entry.Name())
		}
		result = append(result, representationInput{
			ID: entry.Name(), Kind: kind, Timescale: int64(info.Timescale),
			Width: info.Width, Height: info.Height,
			Bandwidth: bandwidthFor(entry.Name(), kind, info.Width, info.Height),
		})
	}
	return result, nil
}

func parseSegmentName(name string) segmentFile {
	file := segmentFile{name: name}
	stem := strings.TrimSuffix(name, ".m4s")
	if strings.HasPrefix(stem, "seg-") {
		if value, err := strconv.ParseInt(strings.TrimPrefix(stem, "seg-"), 10, 64); err == nil {
			file.order, file.numeric = value, true
		}
	}
	return file
}

func sortSegmentFiles(files []segmentFile) {
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].numeric != files[j].numeric {
			return files[i].numeric
		}
		if files[i].numeric && files[i].order != files[j].order {
			return files[i].order < files[j].order
		}
		return files[i].name < files[j].name
	})
}

func loadSegments(root string, rep representationInput) ([]timeline.Segment, error) {
	dir := filepath.Join(root, rep.ID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := make([]segmentFile, 0)
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".m4s") {
			files = append(files, parseSegmentName(entry.Name()))
		}
	}
	sortSegmentFiles(files)
	var result []timeline.Segment
	previousStart := int64(-1)
	segmentIndex := 0
	for _, file := range files {
		path, err := manifest.SafeJoin(root, rep.ID+"/"+file.name)
		if err != nil {
			return nil, manifest.NewError("PATH_ESCAPE", err.Error(), 2, file.name)
		}
		infos, err := mp4.ReadSegmentFile(path, uint32(rep.Timescale))
		if err != nil {
			var parseErr *mp4.ParseError
			if errors.As(err, &parseErr) {
				return nil, manifest.NewError("INPUT_STRUCTURE",
					fmt.Sprintf("parse %s/%s: %v", rep.ID, file.name, err), 2, file.name)
			}
			return nil, err
		}
		for _, info := range infos {
			if int64(info.Timescale) != rep.Timescale {
				return nil, manifest.NewError("INPUT_STRUCTURE",
					fmt.Sprintf("representation %q mixes timescales %d and %d",
						rep.ID, rep.Timescale, info.Timescale), 2, rep.ID)
			}
			if previousStart >= 0 && info.StartTime < previousStart {
				return nil, manifest.NewError("TFDT_ROLLBACK",
					fmt.Sprintf("representation %q has tfdt rollback at %s", rep.ID, file.name),
					2, map[string]any{
						"representation": rep.ID, "segment": file.name,
						"previous": previousStart, "current": info.StartTime,
					})
			}
			previousStart = info.StartTime
			result = append(result, timeline.Segment{
				Start: info.StartTime, Duration: info.Duration,
				ByteStart: info.ByteStart, ByteEnd: info.ByteEnd,
				Path: rep.ID + "/" + file.name,
			})
			segmentIndex++
		}
	}
	if segmentIndex == 0 {
		return nil, manifest.NewError("INPUT_STRUCTURE",
			fmt.Sprintf("representation %q has no readable segments", rep.ID), 2, rep.ID)
	}
	return result, nil
}

func bandwidthFor(id, kind string, width, height uint32) int64 {
	known := map[string]int64{
		"v1080": 5000000, "v720": 3000000, "v480": 1800000, "v360": 1000000,
		"a1": 128000, "a2": 96000,
	}
	if value, ok := known[id]; ok {
		return value
	}
	if kind == "audio" {
		return 64000
	}
	value := int64(width) * int64(height) * 2
	if value < 500000 {
		value = 500000
	}
	return value
}

func alignmentError(report *timeline.Report) error {
	if report.Overlaps > 0 {
		return manifest.NewError("ALIGN_OVERLAP",
			fmt.Sprintf("%d segment overlap(s) exceed tolerance", report.Overlaps),
			3, issueDetails(report, "ALIGN_OVERLAP"))
	}
	if report.Gaps > 0 {
		return manifest.NewError("ALIGN_GAP",
			fmt.Sprintf("%d timeline gap(s) exceed tolerance", report.Gaps),
			3, issueDetails(report, "ALIGN_GAP"))
	}
	if report.MaxDeviationTicks > report.ToleranceTicks {
		return manifest.NewError("ALIGN_DRIFT",
			fmt.Sprintf("maximum deviation %s ms exceeds tolerance",
				timeline.FormatMillis3(report.MaxDeviationTicks, report.TargetTimescale)),
			3, issueDetails(report, "ALIGN_DRIFT"))
	}
	return nil
}

func issueDetails(report *timeline.Report, code string) []map[string]any {
	details := make([]map[string]any, 0)
	for _, issue := range report.Issues {
		if issue.Code != code && code != "ALIGN_DRIFT" {
			continue
		}
		details = append(details, map[string]any{
			"representation": issue.TrackID,
			"index":          issue.Index,
			"deviation_ms":   timeline.FormatMillis3(issue.Amount, issue.Timescale),
		})
	}
	if code == "ALIGN_DRIFT" && len(details) == 0 {
		details = append(details, map[string]any{
			"max_deviation_ms": timeline.FormatMillis3(report.MaxDeviationTicks,
				report.TargetTimescale),
			"tolerance_ms": timeline.FormatMillis3(report.ToleranceTicks,
				report.TargetTimescale),
		})
	}
	return details
}
