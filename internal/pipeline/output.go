package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"aircheck/internal/adapter"
	"aircheck/internal/checkpoint"
	"aircheck/internal/pack"
	"aircheck/internal/render"
)

// digests 检查点绑定的三类摘要。
type digests struct {
	manifest string
	sources  string
	params   string
}

func frameName(i int) string { return fmt.Sprintf("%05d.jpg", i) }

// writeOutputs 写出帧序列、输出清单；支持检查点续跑与模拟中断。
// 返回是否发生了续跑；统计文件由调用方在成功后写出。
func writeOutputs(ctx context.Context, opts Options, m *adapter.Manifest, plan *render.Plan,
	rate *pack.Result, frameSHAs []string, dg digests, outputSHA string) (bool, error) {
	if err := ensureOutDir(opts.OutDir); err != nil {
		return false, err
	}
	store := checkpoint.NewStore(opts.OutDir)
	startAt := 0
	resumed := false
	if opts.Resume {
		st, err := store.Load()
		if err != nil {
			return false, err
		}
		if st == nil {
			return false, fmt.Errorf("pipeline: no checkpoint to resume from")
		}
		if err := checkpoint.Validate(st, dg.manifest, dg.sources, dg.params); err != nil {
			return false, err
		}
		if st.Completed > len(frameSHAs) {
			return false, fmt.Errorf("pipeline: checkpoint completed %d > %d frames", st.Completed, len(frameSHAs))
		}
		// 已写出帧必须与本次确定性结果一致，否则拒绝续跑
		for i := 0; i < st.Completed; i++ {
			data, err := adapter.ReadSecure(opts.OutDir, filepath.Join("frames", frameName(i)), 0)
			if err != nil {
				return false, fmt.Errorf("pipeline: resume verify frame %d: %w", i, err)
			}
			if sha256Hex(data) != st.FrameSHAs[i] || st.FrameSHAs[i] != frameSHAs[i] {
				return false, fmt.Errorf("pipeline: resume frame %d mismatch, refuse resume", i)
			}
		}
		startAt = st.Completed
		resumed = true
	}
	n := len(frameSHAs)
	writeTo := n
	interrupted := false
	if opts.InterruptAfter > 0 && startAt < opts.InterruptAfter && opts.InterruptAfter < n {
		writeTo = opts.InterruptAfter
		interrupted = true
	}
	for i := startAt; i < writeTo; i++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := os.WriteFile(filepath.Join(opts.OutDir, "frames", frameName(i)), rate.Frames[i], 0o644); err != nil {
			return false, fmt.Errorf("pipeline: write frame %d: %w", i, err)
		}
	}
	st := &checkpoint.State{ManifestSHA: dg.manifest, SourcesSHA: dg.sources, ParamsSHA: dg.params,
		Completed: writeTo, FrameSHAs: frameSHAs[:writeTo]}
	if err := store.Save(st); err != nil {
		return false, err
	}
	if interrupted {
		return false, ErrInterrupted
	}
	if err := writeOutManifest(opts.OutDir, m, plan, frameSHAs, outputSHA); err != nil {
		return false, err
	}
	return resumed, nil
}

type outHeader struct {
	Type      string  `json:"type"`
	Version   int     `json:"version"`
	FPS       float64 `json:"fps"`
	Frames    int     `json:"frames"`
	OutputSHA string  `json:"output_sha"`
}

type outFrame struct {
	Type   string  `json:"type"`
	OutSeq int     `json:"out_seq"`
	TimeMs float64 `json:"time_ms"`
	Track  string  `json:"track,omitempty"`
	File   string  `json:"file"`
	SHA256 string  `json:"sha256"`
	Black  bool    `json:"black"`
	GapSrc bool    `json:"gap_src,omitempty"`
}

func writeOutManifest(dir string, m *adapter.Manifest, plan *render.Plan, frameSHAs []string, outputSHA string) error {
	f, err := os.Create(filepath.Join(dir, "out_manifest.jsonl"))
	if err != nil {
		return fmt.Errorf("pipeline: write out manifest: %w", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	if err := enc.Encode(outHeader{Type: "header", Version: m.Version, FPS: plan.FPS,
		Frames: len(frameSHAs), OutputSHA: outputSHA}); err != nil {
		return err
	}
	for i, pf := range plan.Frames {
		rec := outFrame{Type: "frame", OutSeq: pf.OutSeq, TimeMs: pf.TimeMs,
			Track: pf.Track, File: filepath.Join("frames", frameName(i)),
			SHA256: frameSHAs[i], Black: pf.Black, GapSrc: pf.GapSrc}
		if err := enc.Encode(rec); err != nil {
			return err
		}
	}
	return nil
}

type statsFile struct {
	Frames     int               `json:"frames"`
	GapFrames  int               `json:"gap_frames"`
	BadFrames  int               `json:"bad_frames"`
	BadRanges  []render.BadRange `json:"bad_ranges,omitempty"`
	AlignMaxMs float64           `json:"align_max_ms"`
	TargetKbps float64           `json:"target_kbps"`
	ActualKbps float64           `json:"actual_kbps"`
	DeltaPct   float64           `json:"rate_delta_pct"`
	Quality    int               `json:"jpeg_quality"`
	PSNRMeanDB float64           `json:"psnr_mean_db"`
	PSNRMinDB  float64           `json:"psnr_min_db"`
	OutputSHA  string            `json:"output_sha"`
	Resumed    bool              `json:"resumed"`
}

func writeStats(dir string, res *Result) error {
	st := statsFile{
		Frames: res.Frames, GapFrames: res.GapFrames, BadFrames: res.BadFrames,
		BadRanges: res.BadRanges, AlignMaxMs: res.AlignMaxMs,
		TargetKbps: res.Rate.TargetKbps, ActualKbps: res.Rate.ActualKbps,
		DeltaPct: res.Rate.DeltaPct, Quality: res.Rate.Quality,
		PSNRMeanDB: res.Rate.PSNRMeanDB, PSNRMinDB: res.Rate.PSNRMinDB,
		OutputSHA: res.OutputSHA, Resumed: res.Resumed,
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "stats.json"), data, 0o644)
}
