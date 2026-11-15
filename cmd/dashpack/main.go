// dashpack 生成、校验 DASH 清单的命令行工具。
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"dashpack/internal/errs"
)

func main() {
	os.Exit(run())
}

func run() int {
	root := &cobra.Command{
		Use:           "dashpack",
		Short:         "DASH 清单自动组装与校验工具",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newGenFixturesCmd(), newBuildCmd(), newVerifyCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return errs.ExitCode(err)
	}
	return errs.ExitOK
}
