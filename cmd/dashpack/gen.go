package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"dashpack/internal/fixture"
)

func newGenFixturesCmd() *cobra.Command {
	var o fixture.Options
	cmd := &cobra.Command{
		Use:   "gen-fixtures",
		Short: "按种子生成分片夹具与 ladder.json",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := fixture.Generate(o); err != nil {
				return err
			}
			fmt.Printf("fixtures: out=%s reps=%d segments=%d seed=%d\n",
				o.Out, o.Reps, o.Segments, o.Seed)
			return nil
		},
	}
	cmd.Flags().StringVar(&o.Out, "out", "data", "输出目录")
	cmd.Flags().IntVar(&o.Reps, "reps", 4, "表示层总数（含一路音频）")
	cmd.Flags().IntVar(&o.Segments, "segments", 24, "每档分片数")
	cmd.Flags().Int64Var(&o.Seed, "seed", 1, "随机种子")
	return cmd
}
