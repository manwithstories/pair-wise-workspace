// Command archivecheck 是省级媒资恢复验收的纯终端入口。
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"mediaarchive/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(app.Run(ctx, os.Args[1:], os.Stdout))
}
