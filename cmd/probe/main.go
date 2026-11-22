package main

import (
	"bytes"
	"fmt"

	"github.com/abema/go-mp4"
)

func main() {
	// 按 Marshal 产出的真实 v1 布局：flags 后直接 timescale。
	bin := []byte{
		1, 0, 0, 0,
		0x00, 0x00, 0x03, 0xe8, // timescale
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x7b, // pt
		0x00, 0x00, 0x0b, 0xb8, // dur
		0x00, 0x00, 0xab, 0xcd, // id
		'u', 'r', 'n', ':', 't', 0, // scheme
		'f', 'o', 'o', 0, // value
		'a', 'b', 'e', 'm', 'a',
	}
	var dst mp4.Emsg
	_, err := mp4.Unmarshal(bytes.NewReader(bin), uint64(len(bin)), &dst, mp4.Context{})
	fmt.Printf("err=%v scheme=%q value=%q ts=%d pt=%d dur=%d id=%d data=%q\n",
		err, dst.SchemeIdUri, dst.Value, dst.Timescale, dst.PresentationTime,
		dst.EventDuration, dst.Id, dst.MessageData)
}
