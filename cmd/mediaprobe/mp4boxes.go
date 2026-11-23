package main

import (
	"fmt"
	"os"

	mp4 "github.com/abema/go-mp4"
)

// mp4boxes 用 go-mp4 辅助解析媒体结构，列出顶层 box。
func mp4boxes(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mediaprobe mp4boxes FILE")
		return 1
	}
	f, err := os.Open(args[0])
	if err != nil {
		fmt.Printf("status=error\nerror=%q\n", err.Error())
		return 1
	}
	defer f.Close()
	_, err = mp4.ReadBoxStructure(f, func(h *mp4.ReadHandle) (interface{}, error) {
		if len(h.Path) == 1 {
			fmt.Printf("box type=%s offset=%d size=%d\n",
				h.BoxInfo.Type, h.BoxInfo.Offset, h.BoxInfo.Size)
		}
		return nil, nil
	})
	if err != nil {
		fmt.Printf("status=error\nerror=%q\n", err.Error())
		return 1
	}
	return 0
}
