// Package cli 实现 relay 终端命令，把处理内核适配为 cobra 子命令。
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"relay/internal/demux"
	"relay/internal/fixture"
	"relay/internal/media"
	"relay/internal/pipeline"
)

// ExitCode 把内核错误映射为验收协议固定退出码。
func ExitCode(err error) int {
	if err == nil {
		return media.ExitOK
	}
	if errors.Is(err, pipeline.ErrInterrupted) {
		return media.ExitInterrupt
	}
	f := media.AsFault(err)
	if f == nil {
		return media.ExitUsage
	}
	switch f.Stage {
	case media.StageCheckpoint:
		return media.ExitCheckpoint
	case media.StageConfig:
		return media.ExitUsage
	default:
		// prefix/header/length/version/payload/sha/crc/tar 均属已定位媒体损坏。
		return media.ExitMediaCorruption
	}
}

// parseBuffer 解析 64MiB / 1048576 等字节额度写法。
func parseBuffer(s string) (int64, error) {
	s = strings.TrimSpace(s)
	mult := int64(1)
	upper := strings.ToUpper(s)
	for suffix, m := range map[string]int64{
		"KIB": 1 << 10, "MIB": 1 << 20, "GIB": 1 << 30,
		"KB": 1000, "MB": 1000 * 1000, "GB": 1000 * 1000 * 1000,
	} {
		if strings.HasSuffix(upper, suffix) {
			mult = m
			s = s[:len(s)-len(suffix)]
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad buffer size %q", s)
	}
	return n * mult, nil
}

// NewRootCommand 组装全部子命令。
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "relay",
		Short:         "多机位帧序列解复用、轨道映射与无损转封装内核",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newFixtureCmd(), newScanCmd(), newRunCmd(), newResumeCmd(), newDigestCmd())
	return root
}

func newFixtureCmd() *cobra.Command {
	var dir, faults string
	var frames int
	cmd := &cobra.Command{
		Use:   "fixture",
		Short: "生成自包含确定性样例数据",
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := fixture.Generate(fixture.Plan{
				Dir: dir, Frames: frames, Faults: splitCSV(faults),
			})
			if err != nil {
				return err
			}
			return printJSON(map[string]any{
				"dir": dir, "frames": frames,
				"live": out.Live, "map": out.Map,
				"bad_crc": out.BadCRC, "truncated": out.Truncated,
				"conflict": out.Conflict, "beyond_window": out.Beyond,
				"slow": out.Slow,
			})
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "输出目录")
	cmd.Flags().IntVar(&frames, "frames", 1000, "基础帧总数")
	cmd.Flags().StringVar(&faults, "faults", "", "逗号分隔故障标签")
	_ = cmd.MarkFlagRequired("dir")
	return cmd
}

func newScanCmd() *cobra.Command {
	var in string
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "扫描容器并定位首个媒体损坏（退出码 2）",
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(in)
			if err != nil {
				return media.NewFault(media.StageConfig, 0, 0, 0, "open: %v", err)
			}
			defer f.Close()
			if _, err := demux.OpenContainer(f, in); err != nil {
				if flt := media.AsFault(err); flt != nil {
					// 先输出定位坐标，再返回原错误以驱动退出码 2。
					_ = printJSON(map[string]any{
						"record":       flt.Sequence,
						"byte_offset":  flt.ByteOffset,
						"stage":        flt.Stage,
						"camera":       flt.CameraID,
						"offset_error": flt.OffsetErr,
						"error":        flt.Err.Error(),
					})
				}
				return err
			}
			return printJSON(map[string]any{"status": "ok", "byte_offset": 0, "offset_error": 0})
		},
	}
	cmd.Flags().StringVar(&in, "in", "", "输入容器 (.mfs/.mta)")
	_ = cmd.MarkFlagRequired("in")
	return cmd
}

func printJSON(v any) error {
	enc := json.NewEncoder(outWriter)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// outWriter 是所有 JSON 结果的输出目标，测试可替换为缓冲以断言内容。
var outWriter io.Writer = os.Stdout

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
