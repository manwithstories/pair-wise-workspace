package mux

import (
	"bytes"

	"bcastqc/internal/ebml"
)

// VerifyResult 是独立解析器对再封装结果的核对结论。
type VerifyResult struct {
	TrackCount int
	BlockCount int
	// 首块与末块时间（纳秒），用于确认时间线单调且非空。
	FirstTimeNS int64
	LastTimeNS  int64
	Monotonic   bool
	PayloadOK   bool // 逐块负载与输入完全一致
}

// Verify 用独立的 ebml 解读器回读输出文件，
// 并与写入时的期望块逐字节比对，防止自证循环。
func Verify(output []byte, want []MuxBlock) (VerifyResult, error) {
	doc, err := ebml.ParseDocument(ebml.NewBytesReader(output))
	if err != nil {
		return VerifyResult{}, err
	}
	res := VerifyResult{
		TrackCount: len(doc.Tracks),
		BlockCount: len(doc.Blocks),
		Monotonic:  true,
		PayloadOK:  true,
	}
	if len(doc.Blocks) > 0 {
		res.FirstTimeNS = doc.Blocks[0].TimeNS
		res.LastTimeNS = doc.Blocks[len(doc.Blocks)-1].TimeNS
	}
	for i, blk := range doc.Blocks {
		if i > 0 && blk.TimeNS < doc.Blocks[i-1].TimeNS {
			res.Monotonic = false
		}
		if i >= len(want) {
			res.PayloadOK = false
			break
		}
		if blk.Track != want[i].Track || !bytes.Equal(blk.Payload, want[i].Payload) {
			res.PayloadOK = false
		}
	}
	if len(doc.Blocks) != len(want) {
		res.PayloadOK = false
	}
	return res, nil
}
