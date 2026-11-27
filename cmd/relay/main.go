// Command relay 是媒体帧中继处理内核的终端入口。
package main

import (
	"fmt"
	"os"

	"relay/internal/cli"
	"relay/internal/media"
)

func main() {
	root := cli.NewRootCommand()
	err := root.Execute()
	if err == nil {
		os.Exit(media.ExitOK)
	}
	// 内核 Fault / 模拟中断映射为协议退出码；其余按参数错误处理。
	code := cli.ExitCode(err)
	fmt.Fprintln(os.Stderr, "relay:", err)
	os.Exit(code)
}
