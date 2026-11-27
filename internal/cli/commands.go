package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"relay/internal/demux"
	"relay/internal/media"
	"relay/internal/pipeline"
)

// sortStrings 集中排序入口，便于在测试中替换为确定性校验。
func sortStrings(s []string) { sort.Strings(s) }

// runOptions 汇总 run/resume 共用标志。
type runOptions struct {
	in, out, mapPath, checkpointPath, maxBuffer string
	interruptAfter                              int
}

func bindRunFlags(cmd *cobra.Command, o *runOptions) {
	cmd.Flags().StringVar(&o.in, "in", "", "输入 MFS/1 容器")
	cmd.Flags().StringVar(&o.out, "out", "", "输出容器 (.mfs/.mta)")
	cmd.Flags().StringVar(&o.mapPath, "map", "", "机位映射 JSON")
	cmd.Flags().StringVar(&o.checkpointPath, "checkpoint", "", "检查点路径")
	cmd.Flags().StringVar(&o.maxBuffer, "max-buffer", "64MiB", "在途字节硬上限")
	cmd.Flags().IntVar(&o.interruptAfter, "interrupt-after", 0, "提交该帧数后模拟中断")
	for _, name := range []string{"in", "out", "map", "checkpoint"} {
		_ = cmd.MarkFlagRequired(name)
	}
}

func (o runOptions) toPipeline() (pipeline.Options, error) {
	capBytes, err := parseBuffer(o.maxBuffer)
	if err != nil {
		return pipeline.Options{}, media.NewFault(media.StageConfig, 0, 0, 0, "%v", err)
	}
	manifest := o.checkpointPath + ".manifest.json"
	return pipeline.Options{
		InputPath: o.in, OutputPath: o.out, MapPath: o.mapPath,
		CheckpointPath: o.checkpointPath, ManifestPath: manifest,
		MaxBuffer: capBytes, InterruptAfter: o.interruptAfter,
	}, nil
}

func newRunCmd() *cobra.Command {
	o := &runOptions{}
	cmd := &cobra.Command{
		Use:   "run",
		Short: "执行一次完整转封装，可在安全点模拟中断",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := o.toPipeline()
			if err != nil {
				return err
			}
			engine, err := pipeline.New(context.Background(), opts, false)
			if err != nil {
				return err
			}
			err = engine.Run(context.Background())
			stats := engine.Stats()
			_ = printJSON(map[string]any{
				"frames_consumed":   stats.Consumed,
				"frames_released":   stats.Released,
				"duplicates":        stats.Duplicates,
				"quarantined":       stats.Quarantined,
				"peak_buffer_bytes": stats.PeakBuffer,
				"interrupt":         err != nil && err == pipeline.ErrInterrupted,
			})
			return err
		},
	}
	bindRunFlags(cmd, o)
	return cmd
}

func newResumeCmd() *cobra.Command {
	o := &runOptions{}
	cmd := &cobra.Command{
		Use:   "resume",
		Short: "从检查点确定性恢复并跑到完成",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := o.toPipeline()
			if err != nil {
				return err
			}
			start := time.Now()
			engine, err := pipeline.New(context.Background(), opts, true)
			if err != nil {
				return err
			}
			before := engine.Stats().Released
			if err := engine.Run(context.Background()); err != nil {
				return err
			}
			stats := engine.Stats()
			return printJSON(map[string]any{
				"resume_ms":         time.Since(start).Milliseconds(),
				"resumed_frames":    stats.Released - before,
				"frames_released":   stats.Released,
				"peak_buffer_bytes": stats.PeakBuffer,
			})
		},
	}
	bindRunFlags(cmd, o)
	// resume 仅 checkpoint 必填，其余从检查点读取但仍要求显式给出以便校验。
	_ = cmd.Flags().SetAnnotation("checkpoint", cobra.BashCompOneRequiredFlag, []string{"true"})
	return cmd
}

func newDigestCmd() *cobra.Command {
	var in, out string
	cmd := &cobra.Command{
		Use:   "digest",
		Short: "输出容器去重后载荷的确定性摘要",
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(in)
			if err != nil {
				return media.NewFault(media.StageConfig, 0, 0, 0, "open: %v", err)
			}
			frames, err := demux.OpenContainer(f, in)
			f.Close()
			if err != nil {
				return err
			}
			// 规范化：按载荷哈希去重并排序，使输入与转封装输出摘要一致，
			// 不受容器顺序或重复帧影响。
			uniq := map[string]bool{}
			for _, fr := range frames {
				uniq[hex.EncodeToString(fr.SHA256[:])] = true
			}
			hashes := make([]string, 0, len(uniq))
			for h := range uniq {
				hashes = append(hashes, h)
			}
			sortStrings(hashes)
			w, err := os.Create(out)
			if err != nil {
				return media.NewFault(media.StageConfig, 0, 0, 0, "create digest: %v", err)
			}
			defer w.Close()
			root := sha256.New()
			for _, h := range hashes {
				root.Write([]byte(h))
				root.Write([]byte{'\n'})
			}
			digest := root.Sum(nil)
			if _, err := w.Write(digest); err != nil {
				return err
			}
			return printJSON(map[string]any{
				"payload_digest":  hex.EncodeToString(digest),
				"unique_payloads": len(hashes),
			})
		},
	}
	cmd.Flags().StringVar(&in, "in", "", "输入容器")
	cmd.Flags().StringVar(&out, "out", "", "摘要输出文件")
	_ = cmd.MarkFlagRequired("in")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}
