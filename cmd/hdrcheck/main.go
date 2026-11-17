// hdrcheck：纯本地 GARC 容器 HDR 静态元数据解析与一致性校验工具。
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"hdrcheck/internal/app"
	"hdrcheck/internal/fixture"
	"hdrcheck/internal/report"
)

func main() {
	root := &cobra.Command{
		Use:   "hdrcheck",
		Short: "Parse and validate HDR static metadata in GARC media containers",
	}
	root.AddCommand(inspectCmd(), validateCmd(), fixtureCmd())
	if err := root.Execute(); err != nil {
		// cobra 自身的参数错误已经打印过帮助；统一退出码 3。
		os.Exit(app.ExitCorruptOrArgs)
	}
}

func inspectCmd() *cobra.Command {
	var input, jsonOut string
	c := &cobra.Command{
		Use:   "inspect",
		Short: "Parse one GARC file and print its HDR metadata with field provenance",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, code := loadOrExit(input)
			if res == nil {
				os.Exit(code)
			}
			env := report.BuildEnvelope(res.InputName, res.Report)
			if jsonOut != "" {
				if err := writeJSONFile(jsonOut, env); err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(app.ExitInternal)
				}
			}
			if err := report.WriteText(os.Stdout, env); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(app.ExitInternal)
			}
			os.Exit(app.DecideExitCode(res.Report, false))
			return nil
		},
	}
	c.Flags().StringVar(&input, "input", "", "path to the GARC file (required)")
	c.Flags().StringVar(&jsonOut, "json", "", "optional path to also write the stable JSON report")
	_ = c.MarkFlagRequired("input")
	return c
}

func validateCmd() *cobra.Command {
	var input string
	var strict bool
	c := &cobra.Command{
		Use:   "validate",
		Short: "Run cross-source and cross-track consistency checks",
		Long: "Audit mode (default) prints every issue and keeps valid fields, exiting 0 " +
			"for warning-only results. Strict mode (--strict) exits 2 on any business conflict.",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, code := loadOrExit(input)
			if res == nil {
				os.Exit(code)
			}
			env := report.BuildEnvelope(res.InputName, res.Report)
			if err := report.WriteText(os.Stdout, env); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(app.ExitInternal)
			}
			os.Exit(app.DecideExitCode(res.Report, strict))
			return nil
		},
	}
	c.Flags().StringVar(&input, "input", "", "path to the GARC file (required)")
	c.Flags().BoolVar(&strict, "strict", false, "exit 2 on any business conflict or missing field")
	_ = c.MarkFlagRequired("input")
	return c
}

func fixtureCmd() *cobra.Command {
	var out string
	c := &cobra.Command{
		Use:   "fixture",
		Short: "Generate all local GARC test fixtures (no external media)",
		RunE: func(cmd *cobra.Command, args []string) error {
			names, err := fixture.GenerateAll(out)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(app.ExitInternal)
			}
			fmt.Printf("generated %d fixtures under %s:\n", len(names), out)
			for _, n := range names {
				fmt.Printf("  - %s\n", n)
			}
			return nil
		},
	}
	c.Flags().StringVar(&out, "out", "./fixtures", "output directory for generated fixtures")
	return c
}

// loadOrExit 执行流水线；失败时打印结构化错误并返回 (nil, 退出码)。
func loadOrExit(input string) (*app.PipelineResult, int) {
	res, err := app.Run(input)
	if err != nil {
		if app.IsCorruptError(err) {
			fmt.Fprintf(os.Stderr, "hdrcheck: %v\n", err)
			return nil, app.ExitCorruptOrArgs
		}
		fmt.Fprintf(os.Stderr, "hdrcheck internal error: %v\n", err)
		return nil, app.ExitInternal
	}
	return res, app.ExitOK
}

func writeJSONFile(path string, env report.Envelope) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("cannot create %s: %w", path, err)
	}
	defer f.Close()
	return report.WriteJSON(f, env)
}
