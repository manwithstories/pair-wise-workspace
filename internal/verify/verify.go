package verify

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"m3u8ctl/internal/playlist"
	"m3u8ctl/internal/ts"
)

// Config controls manifest-to-disk verification.
type Config struct {
	Root        string
	Tolerance   time.Duration
	Concurrency int
}

type segmentState struct {
	index         int
	uri           string
	path          string
	sequence      int64
	declared      time.Duration
	discontinuity bool
	exists        bool
	size          int64
	probe         *ts.Probe
	duration      time.Duration
	validTimeline bool
}

type variantData struct {
	report       VariantReport
	media        *playlist.MediaPlaylist
	segments     []segmentState
	bandwidthBPS int64
	averageBPS   int64
}

// Run cross-checks the master playlist against the local segment tree.
func Run(cfg Config) (*Report, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("root is required")
	}
	if cfg.Tolerance < 0 {
		return nil, fmt.Errorf("tolerance must not be negative")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	masterPath, baseDir, err := locateMaster(cfg.Root)
	if err != nil {
		return nil, err
	}
	master, err := playlist.ParseMasterFile(masterPath)
	if err != nil {
		return nil, fmt.Errorf("parse master: %w", err)
	}
	data := make([]*variantData, len(master.Variants))
	group, ctx := errgroup.WithContext(context.Background())
	for i, variant := range master.Variants {
		i, variant := i, variant
		group.Go(func() error {
			data[i] = verifyVariant(ctx, cfg, baseDir, variant)
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	report := &Report{Root: cfg.Root}
	for _, item := range data {
		report.Variants = append(report.Variants, item.report)
	}
	addCrossVariantFailures(data, cfg.Tolerance)
	report.Variants = report.Variants[:0]
	for _, item := range data {
		report.Variants = append(report.Variants, item.report)
	}
	report.Finalize()
	return report, nil
}

func locateMaster(root string) (string, string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return "", "", err
	}
	if info.IsDir() {
		return filepath.Join(root, "master.m3u8"), root, nil
	}
	return root, filepath.Dir(root), nil
}

func verifyVariant(ctx context.Context, cfg Config, baseDir string, variant playlist.Variant) *variantData {
	out := &variantData{report: VariantReport{
		Name:                  variant.Name,
		BitrateAdvertisedKbps: variant.Bandwidth / 1000,
		Failures:              []Failure{},
	}, bandwidthBPS: variant.Bandwidth, averageBPS: variant.AverageBandwidth}
	playlistPath := localPath(baseDir, variant.URI)
	out.report.Playlist = playlistPath
	media, err := playlist.ParseMediaFile(playlistPath)
	if err != nil {
		addFailure(out, "PLAYLIST_INVALID", 0, variant.URI, "cannot parse media playlist: "+err.Error(), "valid playlist", "error")
		return out
	}
	out.media = media
	out.report.SegmentsExpected = len(media.Segments)
	variantDir := filepath.Dir(playlistPath)
	out.segments = make([]segmentState, len(media.Segments))
	referenced := make(map[string]bool, len(media.Segments))
	for i, segment := range media.Segments {
		state := segmentState{
			index: i, uri: segment.URI, sequence: media.MediaSequence + int64(i),
			declared: durationSeconds(segment.Duration), discontinuity: segment.Discontinuity,
		}
		state.path = localPath(variantDir, segment.URI)
		referenced[filepath.Clean(state.path)] = true
		info, statErr := os.Stat(state.path)
		if statErr == nil && !info.IsDir() {
			state.exists = true
			state.size = info.Size()
			out.report.SegmentsFound++
		}
		out.segments[i] = state
	}
	recordMissingIntervals(out)
	recordDiskExtras(out, variantDir, referenced)
	for i := range out.segments {
		state := &out.segments[i]
		select {
		case <-ctx.Done():
			return out
		default:
		}
		if !state.exists {
			continue
		}
		if state.size == 0 {
			addFailure(out, "SEG_EMPTY", state.sequence, state.uri, "segment is zero bytes", ">0 bytes", "0 bytes")
			continue
		}
		probe, probeErr := ts.ProbeFile(state.path)
		if probeErr != nil {
			addFailure(out, "TS_TRUNCATED", state.sequence, state.uri, probeErr.Error(), "188-byte TS packets", "unreadable")
			continue
		}
		state.probe = probe
		if state.size%ts.PacketSize != 0 || probe.TailBytes != 0 || probe.Truncated || probe.SyncErrors > 0 {
			addFailure(out, "TS_TRUNCATED", state.sequence, state.uri,
				"segment is not a whole number of 188-byte packets", "188-byte aligned", fmt.Sprintf("%d bytes", state.size))
		}
		if !probe.HasPCR {
			addFailure(out, "PCR_MISSING", state.sequence, state.uri, "no PCR found", "PCR", "none")
			continue
		}
		state.duration = probe.Duration()
		state.validTimeline = state.duration > 0
	}
	recordTimelineFailures(out, time.Duration(media.TargetDuration)*time.Second)
	recordDurationMetrics(out, cfg.Tolerance)
	recordBandwidth(out)
	return out
}

func recordMissingIntervals(out *variantData) {
	for i := 0; i < len(out.segments); {
		if out.segments[i].exists {
			i++
			continue
		}
		start := i
		for i < len(out.segments) && !out.segments[i].exists {
			i++
		}
		first := out.segments[start]
		last := out.segments[i-1]
		count := i - start
		addFailure(out, "SEG_GAP", first.sequence, first.uri,
			fmt.Sprintf("%d consecutive referenced segment(s) missing", count),
			strconv.Itoa(count), "0")
		if count > 1 {
			out.report.Failures[len(out.report.Failures)-1].EndSequence = last.sequence
		}
	}
}

func recordDiskExtras(out *variantData, variantDir string, referenced map[string]bool) {
	entries, err := os.ReadDir(variantDir)
	if err != nil {
		return
	}
	maxSequence := out.media.MediaSequence + int64(len(out.media.Segments))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".ts") {
			continue
		}
		path := filepath.Join(variantDir, entry.Name())
		if referenced[filepath.Clean(path)] {
			continue
		}
		addFailure(out, "DISK_EXTRA", maxSequence, entry.Name(), "segment exists on disk but is not referenced", "0 references", "1")
		maxSequence++
	}
}

func recordTimelineFailures(out *variantData, target time.Duration) {
	var prev *segmentState
	seen := make(map[int64]bool)
	var lastSourceSequence int64 = -1
	for i := range out.segments {
		state := &out.segments[i]
		if sourceSequence, ok := decodeSequence(state.uri); ok {
			if lastSourceSequence >= 0 && sourceSequence < lastSourceSequence {
				addFailure(out, "SEG_ORDER", state.sequence, state.uri, "decoded sequence decreases", "monotonic", strconv.FormatInt(sourceSequence, 10))
			}
			lastSourceSequence = sourceSequence
		}
		if !state.validTimeline {
			continue
		}
		if seen[state.probe.FirstPCR] {
			addFailure(out, "SEG_DUP", state.sequence, state.uri, "duplicate PCR start", "unique PCR", strconv.FormatInt(state.probe.FirstPCR, 10))
		} else if prev != nil {
			if state.probe.FirstPCR < prev.probe.LastPCR {
				addFailure(out, "PCR_ROLLBACK", state.sequence, state.uri, "PCR moved backwards", strconv.FormatInt(prev.probe.LastPCR, 10), strconv.FormatInt(state.probe.FirstPCR, 10))
			} else {
				gap := ts.TicksToDuration(state.probe.FirstPCR - prev.probe.LastPCR)
				missingReferenceBetween := state.index > prev.index+1
				if gap > time.Second && gap > target/2 && !state.discontinuity && !missingReferenceBetween {
					addFailure(out, "SEG_GAP", state.sequence, state.uri, "PCR gap without discontinuity", "<= target/2", gap.String())
				}
				if gap > 10*time.Second {
					addFailure(out, "PCR_JUMP", state.sequence, state.uri, "PCR jump exceeds 10 seconds", "<=10s", gap.String())
				}
			}
		}
		seen[state.probe.FirstPCR] = true
		prev = state
	}
}

func recordDurationMetrics(out *variantData, tolerance time.Duration) {
	target := time.Duration(out.media.TargetDuration) * time.Second
	var maxDev, totalDev time.Duration
	var count int
	for i := range out.segments {
		state := &out.segments[i]
		if !state.validTimeline {
			continue
		}
		devDeclared := absDuration(state.probe.Duration() - state.declared)
		devTarget := absDuration(state.probe.Duration() - target)
		dev := devDeclared
		if devTarget > dev {
			dev = devTarget
		}
		if dev > maxDev {
			maxDev = dev
		}
		totalDev += dev
		count++
		if dev > tolerance {
			addFailure(out, "EXTINF_DRIFT", state.sequence, state.uri,
				"EXTINF differs from measured PCR duration", target.String(), state.probe.Duration().String())
		}
	}
	if count > 0 {
		out.report.DurationDevMaxMS = durationMS(maxDev)
		out.report.DurationDevMeanMS = durationMS(totalDev / time.Duration(count))
	}
}

func recordBandwidth(out *variantData) {
	var bytes int64
	var total time.Duration
	values := make([]bandwidthSegment, 0, len(out.segments))
	for _, state := range out.segments {
		duration := state.duration
		if duration <= 0 {
			duration = state.declared
		}
		bytes += state.size
		total += duration
		values = append(values, bandwidthSegment{bytes: state.size, duration: duration})
	}
	peak := peakBandwidth(values)
	average := bps(bytes, total)
	out.report.BitrateMeasuredKbps = average / 1000
	if peak != out.bandwidthBPS {
		addFailure(out, "BANDWIDTH_MISMATCH", out.media.MediaSequence, "BANDWIDTH",
			"advertised peak bandwidth does not match disk", strconv.FormatInt(peak/1000, 10)+" kbps", strconv.FormatInt(out.report.BitrateAdvertisedKbps, 10)+" kbps")
	}
	if average != out.averageBPS {
		addFailure(out, "BANDWIDTH_MISMATCH", out.media.MediaSequence, "AVERAGE-BANDWIDTH",
			"advertised average bandwidth does not match disk", strconv.FormatInt(average/1000, 10)+" kbps", strconv.FormatInt(out.averageBPS/1000, 10)+" kbps")
	}
}

func addFailure(out *variantData, code string, sequence int64, uri, message, expected, actual string) {
	out.report.Failures = append(out.report.Failures, Failure{
		Code: code, Variant: out.report.Name, Sequence: sequence, URI: uri,
		Message: message, Expected: expected, Actual: actual,
	})
}

func decodeSequence(uri string) (int64, bool) {
	base := strings.TrimSuffix(filepath.Base(strings.SplitN(uri, "?", 2)[0]), filepath.Ext(uri))
	digits := 0
	for digits < len(base) && base[digits] >= '0' && base[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return 0, false
	}
	value, err := strconv.ParseInt(base[:digits], 10, 64)
	return value, err == nil
}

func localPath(base, uri string) string {
	clean := strings.SplitN(uri, "?", 2)[0]
	if strings.Contains(clean, "://") {
		return clean
	}
	return filepath.Join(base, filepath.FromSlash(clean))
}

func durationSeconds(value float64) time.Duration {
	return time.Duration(math.Round(value * float64(time.Second)))
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func durationMS(value time.Duration) int64 {
	return int64(math.Round(float64(value) / float64(time.Millisecond)))
}

type bandwidthSegment struct {
	bytes    int64
	duration time.Duration
}

func bps(bytes int64, duration time.Duration) int64 {
	if bytes <= 0 || duration <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(bytes*8) / duration.Seconds()))
}

func peakBandwidth(values []bandwidthSegment) int64 {
	var peak int64
	for i := range values {
		end := i + 3
		if end > len(values) {
			end = len(values)
		}
		if end-i < 3 && len(values) >= 3 {
			continue
		}
		var bytes int64
		var duration time.Duration
		for _, value := range values[i:end] {
			bytes += value.bytes
			duration += value.duration
		}
		if value := bps(bytes, duration); value > peak {
			peak = value
		}
	}
	return peak
}
