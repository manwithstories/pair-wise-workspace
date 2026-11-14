package gen

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"m3u8ctl/internal/playlist"
	"m3u8ctl/internal/ts"
)

// Config controls playlist generation.
type Config struct {
	Input          string
	Output         string
	Window         int
	TargetDuration time.Duration
	EndList        bool
	Concurrency    int
}

// VariantResult summarizes one generated rendition.
type VariantResult struct {
	Name             string
	Playlist         string
	Segments         int
	Bandwidth        int64
	AverageBandwidth int64
}

// Result summarizes all generated output.
type Result struct {
	Master   string
	Variants []VariantResult
}

type sourceSegment struct {
	path     string
	name     string
	sequence int64
	probe    *ts.Probe
	duration time.Duration
}

var renditionOrder = map[string]int{"1080p": 0, "720p": 1, "480p": 2}

// Generate creates master.m3u8 and one media playlist per rendition.
func Generate(cfg Config) (*Result, error) {
	if cfg.Input == "" || cfg.Output == "" {
		return nil, fmt.Errorf("input and output are required")
	}
	if cfg.TargetDuration <= 0 {
		return nil, fmt.Errorf("target duration must be positive")
	}
	if cfg.Window < 0 {
		return nil, fmt.Errorf("window must not be negative")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	entries, err := os.ReadDir(cfg.Input)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool {
		oi, iok := renditionOrder[names[i]]
		oj, jok := renditionOrder[names[j]]
		if iok && jok {
			return oi < oj
		}
		return names[i] < names[j]
	})
	if len(names) == 0 {
		return nil, fmt.Errorf("no rendition directories under %s", cfg.Input)
	}
	if err := prepareOutput(cfg.Input, cfg.Output); err != nil {
		return nil, err
	}
	result := &Result{Master: filepath.Join(cfg.Output, "master.m3u8")}
	master := &playlist.MasterPlaylist{Version: 3, IndependentSegments: true}
	var mu sync.Mutex
	group, ctx := errgroup.WithContext(context.Background())
	for _, name := range names {
		name := name
		group.Go(func() error {
			variant, err := generateVariant(ctx, cfg, name)
			if err != nil {
				return err
			}
			mu.Lock()
			result.Variants = append(result.Variants, variant.result)
			mu.Unlock()
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, fmt.Errorf("generate variants: %w", err)
	}
	sort.Slice(result.Variants, func(i, j int) bool {
		return renditionRank(result.Variants[i].Name) < renditionRank(result.Variants[j].Name)
	})
	for _, variant := range result.Variants {
		master.Variants = append(master.Variants, playlist.Variant{
			Name: variant.Name, URI: variant.Name + "/index.m3u8",
			Bandwidth: variant.Bandwidth, AverageBandwidth: variant.AverageBandwidth,
			Resolution: resolution(variant.Name),
		})
	}
	f, err := os.Create(result.Master)
	if err != nil {
		return nil, err
	}
	if err := master.Write(f); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return result, nil
}

type variantOutput struct{ result VariantResult }

func generateVariant(ctx context.Context, cfg Config, name string) (*variantOutput, error) {
	entries, err := os.ReadDir(filepath.Join(cfg.Input, name))
	if err != nil {
		return nil, err
	}
	segments := make([]sourceSegment, 0, len(entries))
	var mu sync.Mutex
	probeGroup, _ := errgroup.WithContext(ctx)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".ts") {
			continue
		}
		entry := entry
		probeGroup.Go(func() error {
			path := filepath.Join(cfg.Input, name, entry.Name())
			probe, err := ts.ProbeFile(path)
			if err != nil {
				return err
			}
			duration := probe.Duration()
			if duration <= 0 {
				duration = cfg.TargetDuration
			}
			seg := sourceSegment{path: path, name: entry.Name(), sequence: decodeSequence(entry.Name()), probe: probe, duration: duration}
			mu.Lock()
			segments = append(segments, seg)
			mu.Unlock()
			return nil
		})
	}
	if err := probeGroup.Wait(); err != nil {
		return nil, fmt.Errorf("probe %s: %w", name, err)
	}
	if len(segments) == 0 {
		return nil, fmt.Errorf("no TS segments in %s", name)
	}
	sort.Slice(segments, func(i, j int) bool {
		a, b := segments[i], segments[j]
		if a.probe.HasPCR != b.probe.HasPCR {
			return a.probe.HasPCR
		}
		if a.probe.HasPCR && a.probe.FirstPCR != b.probe.FirstPCR {
			return a.probe.FirstPCR < b.probe.FirstPCR
		}
		if a.sequence != b.sequence {
			return a.sequence < b.sequence
		}
		return a.name < b.name
	})
	window, err := playlist.NewWindow(cfg.TargetDuration, cfg.Window, 0)
	if err != nil {
		return nil, err
	}
	for _, seg := range segments {
		window.Add(playlist.WindowSegment{
			URI: seg.name, Duration: seg.duration, FirstPCR: seg.probe.FirstPCR,
			LastPCR: seg.probe.LastPCR, FrameInterval: seg.probe.FrameInterval,
			Size: seg.probe.Size, SourceSequence: seg.sequence,
		})
	}
	kept := window.Segments()
	outDir := filepath.Join(cfg.Output, name)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	byName := make(map[string]sourceSegment, len(segments))
	for _, seg := range segments {
		byName[seg.name] = seg
	}
	for _, seg := range kept {
		src := byName[seg.URI]
		if err := copySegment(ctx, src.path, filepath.Join(outDir, seg.URI)); err != nil {
			return nil, fmt.Errorf("copy %s: %w", src.name, err)
		}
	}
	media := window.Playlist(cfg.EndList)
	media.TargetDuration = int(math.Ceil(cfg.TargetDuration.Seconds()))
	f, err := os.Create(filepath.Join(outDir, "index.m3u8"))
	if err != nil {
		return nil, err
	}
	if err := media.Write(f); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	bandwidth, average := calculateBandwidth(kept)
	return &variantOutput{result: VariantResult{
		Name: name, Playlist: filepath.Join(outDir, "index.m3u8"), Segments: len(kept),
		Bandwidth: bandwidth, AverageBandwidth: average,
	}}, nil
}

func calculateBandwidth(segments []playlist.WindowSegment) (int64, int64) {
	var totalBytes int64
	var totalDuration time.Duration
	for _, seg := range segments {
		totalBytes += seg.Size
		totalDuration += seg.Duration
	}
	average := ceilBPS(totalBytes, totalDuration)
	var peak int64
	for i := 0; i < len(segments); i++ {
		end := i + 3
		if end > len(segments) {
			end = len(segments)
		}
		if end-i < 3 && len(segments) >= 3 {
			continue
		}
		var bytes int64
		var duration time.Duration
		for _, seg := range segments[i:end] {
			bytes += seg.Size
			duration += seg.Duration
		}
		if bps := ceilBPS(bytes, duration); bps > peak {
			peak = bps
		}
	}
	return peak, average
}

func ceilBPS(bytes int64, duration time.Duration) int64 {
	if bytes <= 0 || duration <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(bytes*8) / duration.Seconds()))
}

func copySegment(ctx context.Context, src, dst string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func prepareOutput(input, output string) error {
	inAbs, err := filepath.Abs(input)
	if err != nil {
		return err
	}
	outAbs, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	if outAbs == "/" || outAbs == inAbs || strings.HasPrefix(inAbs+string(filepath.Separator), outAbs+string(filepath.Separator)) {
		return fmt.Errorf("refusing to replace output path %s", output)
	}
	if err := os.RemoveAll(output); err != nil {
		return err
	}
	return os.MkdirAll(output, 0o755)
}

func decodeSequence(name string) int64 {
	base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	digits := 0
	for digits < len(base) && base[digits] >= '0' && base[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return -1
	}
	v, err := strconv.ParseInt(base[:digits], 10, 64)
	if err != nil {
		return -1
	}
	return v
}

func resolution(name string) string {
	switch name {
	case "1080p":
		return "1920x1080"
	case "720p":
		return "1280x720"
	case "480p":
		return "854x480"
	default:
		return ""
	}
}

func renditionRank(name string) int {
	if rank, ok := renditionOrder[name]; ok {
		return rank
	}
	return len(renditionOrder)
}
