// Package ingest 校验输入帧序列并执行单表示层独立恢复。
// 完整性故障（哈希不符、元数据缺失、时间戳倒退）按帧定位，
// 发生后保留可信前缀并报告首个错误位置；可用性故障的恢复区间
// 由 transport 层提供，这里只做合并与标记。
package ingest

import (
	"crypto/sha256"
	"fmt"
	"time"

	"archivecheck/internal/clock"
	"archivecheck/internal/model"
)

// FirstError 定位首个校验失败的帧。
type FirstError struct {
	Seq   uint32
	Fault model.FaultCode
}

func (e FirstError) Error() string {
	return fmt.Sprintf("ingest: 帧 %d 校验失败: %s", e.Seq, e.Fault)
}

// Reader 顺序校验一个表示层的帧序列，内存随滑动窗口有界。
type Reader struct {
	rep       string
	unwrap    *clock.Unwrapper
	lastSeq   uint32
	started   bool
	prefix    []model.Frame // 可信前缀
	firstErr  *FirstError
	backwards int
	// OnVerified 在每帧通过全部校验后回调，参数为该帧的端到端处理时延（纳秒）。
	OnVerified func(seq uint32, latencyNS int64)
}

// NewReader 构造校验读取器，wrapMod 为时间戳回绕模。
func NewReader(rep string, wrapMod uint64) *Reader {
	return &Reader{rep: rep, unwrap: clock.NewUnwrapper(wrapMod)}
}

// Verify 逐帧校验。返回可信前缀与首个错误（无错误时为 nil）。
// 一旦出错立即停止，后续帧不再进入可信前缀，保证前缀全部可复核。
func (r *Reader) Verify(frames []model.Frame) ([]model.Frame, *FirstError) {
	for _, f := range frames {
		start := time.Now()
		if f.Rep == "" || len(f.PayloadHash) != sha256.Size {
			r.fail(f.Seq, model.FaultMetaMissing)
			break
		}
		sum := sha256.Sum256(f.Payload)
		if string(sum[:]) != string(f.PayloadHash) {
			r.fail(f.Seq, model.FaultHashMismatch)
			break
		}
		if r.started && f.Seq != r.lastSeq+1 {
			r.fail(f.Seq, model.FaultMetaMissing) // 序号断档按元数据缺失处理
			break
		}
		if _, backward := r.unwrap.Extend(f.RTPTimestamp); backward {
			r.backwards++
			r.fail(f.Seq, model.FaultClockBack)
			break
		}
		if r.OnVerified != nil {
			r.OnVerified(f.Seq, model.SinceNS(start))
		}
		r.started = true
		r.lastSeq = f.Seq
		r.prefix = append(r.prefix, f)
	}
	return r.prefix, r.firstErr
}

func (r *Reader) fail(seq uint32, code model.FaultCode) {
	if r.firstErr == nil {
		r.firstErr = &FirstError{Seq: seq, Fault: code}
	}
}

// Backwards 返回检测到的时间戳倒退次数（仅在完整通过时有意义）。
func (r *Reader) Backwards() int { return r.backwards }
