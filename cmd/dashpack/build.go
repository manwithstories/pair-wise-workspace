package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"dashpack/internal/align"
	"dashpack/internal/errs"
	"dashpack/internal/manifest"
	"dashpack/internal/packager"
	"dashpack/internal/report"
	"dashpack/internal/source"
)

func newBuildCmd() *cobra.Command {
	var src, mpdPath, reportPath, policy string
	cmd := &cobra.Command{
		Use:   "build",
		Short: "扫描分片、对齐时间轴并组装 MPD 与对齐报告",
		RunE: func(cmd *cobra.Command, args []string) error {
			reps, err := source.Scan(src)
			if err != nil {
				return err
			}
			aligned, err := align.Align(reps, policy)
			if err != nil {
				return err
			}
			m, info, err := packager.Build(src, reps, aligned)
			if err != nil {
				return err
			}
			data, err := manifest.Marshal(m)
			if err != nil {
				return err
			}
			if err := writeAtomic(mpdPath, data); err != nil {
				return err
			}
			rep := report.FromAlignment(aligned, info.PeriodDurationMs,
				info.Repairs, info.RepairedItems)
			if err := report.Write(reportPath, rep); err != nil {
				return err
			}
			fmt.Println(report.Summary(rep))
			fmt.Printf("mpd: %s\nreport: %s\n", mpdPath, reportPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&src, "source", "data", "分片源目录")
	cmd.Flags().StringVar(&mpdPath, "mpd", "out.mpd", "MPD 输出路径")
	cmd.Flags().StringVar(&reportPath, "report", "alignment.json", "对齐报告输出路径")
	cmd.Flags().StringVar(&policy, "policy", align.PolicyTolerant, "对齐策略：strict|tolerant")
	return cmd
}

// writeAtomic 先写临时文件再改名，失败不留半成品。
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return errs.New(errs.EWrite, errs.ExitWrite, "write %s: %v", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return errs.New(errs.EWrite, errs.ExitWrite, "rename %s: %v", path, err)
	}
	return nil
}
