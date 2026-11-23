package mux

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"media-pipeline/internal/fault"
)

// VerifyReadback 回读 fMP4，校验 box 结构并提取 mdat 载荷与原始 PCM 比对，
// 返回载荷的 SHA-256。任何结构或字节不一致都报内部处理失败。
func VerifyReadback(r io.ReadSeeker, wantPCM []byte) ([32]byte, error) {
	var payload bytes.Buffer
	var sawMdat, sawMoof, sawTfdt bool
	var tfdtBase uint64
	_, err := mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case mp4.BoxTypeMoof():
			sawMoof = true
			return h.Expand()
		case mp4.BoxTypeTfdt():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			tfdtBase = box.(*mp4.Tfdt).GetBaseMediaDecodeTime()
			sawTfdt = true
			return nil, nil
		case mp4.BoxTypeMdat():
			sawMdat = true
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			payload.Write(box.(*mp4.Mdat).Data)
			return nil, nil
		default:
			if len(h.Path) > 0 {
				return h.Expand()
			}
			return nil, nil
		}
	})
	if err != nil {
		return [32]byte{}, wrapErr(err)
	}
	if !sawMoof || !sawMdat || !sawTfdt {
		return [32]byte{}, fault.New(fault.KindOther, "", -1, int64(tfdtBase), false,
			"readback missing moof/tfdt/mdat box").WithOp("mux.verify")
	}
	if !bytes.Equal(payload.Bytes(), wantPCM) {
		return [32]byte{}, fault.New(fault.KindOther, "", -1, -1, false,
			fmt.Sprintf("mdat payload %d bytes differs from source PCM %d bytes",
				payload.Len(), len(wantPCM))).WithOp("mux.verify")
	}
	return sha256.Sum256(payload.Bytes()), nil
}

func wrapErr(err error) error {
	if fe, ok := fault.As(err); ok {
		return fe
	}
	return fault.New(fault.KindOther, "", -1, -1, false,
		"fMP4 muxing failed").WithOp("mux").Wrap(err)
}
