package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"dashpack/internal/packager"
)

func newVerifyCmd() *cobra.Command {
	var mpdPath, src string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "反向解析 MPD 并展开模板回查源目录文件",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := packager.Verify(mpdPath, src)
			if err != nil {
				return err
			}
			if asJSON {
				data, merr := json.Marshal(res)
				if merr != nil {
					return merr
				}
				fmt.Println(string(data))
			} else {
				fmt.Printf("verify ok: representations=%d adaptation_sets=%d "+
					"segments_expanded=%d period_duration_ms=%d "+
					"max_deviation_ticks=%d max_deviation_ms=%.3f "+
					"repaired_items=%d files_missing=%d\n",
					res.Representations, res.AdaptationSets, res.SegmentsExpanded,
					res.PeriodDurationMs, res.MaxDeviationTicks,
					float64(res.MaxDeviationMs), res.RepairedItems, res.FilesMissing)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&mpdPath, "mpd", "out.mpd", "MPD 路径")
	cmd.Flags().StringVar(&src, "source", "data", "分片源目录")
	cmd.Flags().BoolVar(&asJSON, "json", false, "以 JSON 输出校验结果")
	return cmd
}
