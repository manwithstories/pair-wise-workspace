package engine

import (
	"encoding/binary"
	"io"
	"os"
	"strconv"
	"time"
	"unsafe"

	"github.com/asticode/go-astiav"

	"relay/internal/audio"
	"relay/internal/index"
	"relay/internal/media"
)

// binaryBE 读取 4 字节大端无符号整数。
func binaryBE(b []byte) uint32 { return binary.BigEndian.Uint32(b) }

// render 采用两遍分离，保证中断/写盘失败后续跑产物与完整运行字节一致：
//  1. measure：完整解码重采样并测积分响度，得到确定性增益（不写检查点进度）；
//  2. render：从检查点进度开始，施加固定增益、真峰值限制并打包 s16 写出。
//
// 中断与写盘失败只可能发生在 render 阶段；measure 阶段对故障输入插入静音占位，
// 使时间轴与样本计数确定。
func render(cfg Config, idx *index.Index, inputDigest string,
	cpStore *CheckpointStore, rep *Report) (float64, float64, error) {

	dec, err := audio.NewDecoder(cfg.Params.AudioRate, cfg.Params.Channels)
	if err != nil {
		if audio.IsUnsupported(err) {
			return 0, 0, &media.UnsupportedError{Reason: err.Error()}
		}
		return 0, 0, err
	}
	norm, err := audio.NewNormalizer(cfg.Params.AudioRate, cfg.Params.Channels)
	if err != nil {
		return 0, 0, err
	}
	defer norm.Free()

	// 坏包定位：索引已在扫描期记录带序号的可恢复故障，取首个可识别序号。
	for i := range idx.Faults {
		if f := idx.Faults[i]; f.Seq != media.SeqUnknown {
			repLocate(rep, f.Seq)
			break
		}
	}

	stagePath := cfg.WorkDir + "/stage.f32"
	gainPath := cfg.WorkDir + "/gain.json"

	// 第一遍：完整测量并生成暂存（幂等，每次重建 stage 以保证确定性）。
	gain, lufs, err := measurePass(cfg, dec, norm, idx, stagePath, gainPath)
	if err != nil {
		return 0, 0, err
	}

	// 第二遍：检查点驱动渲染，可中断/续跑。
	cp, _ := cpStore.Load()
	if cp != nil && !cp.Compatible(inputDigest, cfg.Params.Digest()) {
		cp = nil
		_ = cpStore.Remove()
	}
	if err := renderPassResumable(cfg, idx, stagePath, gain, cpStore, inputDigest, cp); err != nil {
		return 0, 0, err
	}
	return gain, lufs, nil
}

// measurePass 完整跑一遍解码+重采样，测响度并把 fltp 立体声写入暂存，
// 同时把确定性增益落盘。故障包写静音占位，保证样本计数可复现。
func measurePass(cfg Config, dec *audio.Decoder, norm *audio.Normalizer,
	idx *index.Index, stagePath, gainPath string) (float64, float64, error) {

	in, err := os.Open(cfg.InputPath)
	if err != nil {
		return 0, 0, &media.IOError{Op: "open", Path: cfg.InputPath, Reason: err.Error()}
	}
	defer in.Close()
	stage, err := os.OpenFile(stagePath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, 0, &media.IOError{Op: "open", Path: stagePath, Reason: err.Error()}
	}
	defer stage.Close()

	pr := newPacketReader(in)
	meter := audio.NewLoudnessMeter()
	silence := make([]byte, cfg.Params.AudioRate/100*cfg.Params.Channels*2)

	for i := range idx.Entries {
		payload, perr := pr.payloadAt(idx.Entries[i])
		if perr != nil {
			payload = silence // 坏包：静音占位，保持时间轴连续
		}
		src, derr := dec.Decode(payload)
		if derr != nil {
			if audio.IsBadPayload(derr) {
				src, derr = dec.Decode(silence)
			}
			if derr != nil {
				return 0, 0, derr
			}
		}
		dst, nerr := norm.Process(src)
		src.Free()
		if nerr != nil {
			return 0, 0, nerr
		}
		meter.Observe(dst)
		if err := writeStageFrame(stage, dst); err != nil {
			dst.Free()
			return 0, 0, err
		}
		dst.Free()
	}
	if flush, ferr := norm.Flush(); ferr != nil {
		return 0, 0, ferr
	} else if flush != nil {
		meter.Observe(flush)
		_ = writeStageFrame(stage, flush)
		flush.Free()
	}
	gain, lufs := meter.GainDB(), meter.LUFS()
	if err := writeGain(gainPath, gain); err != nil {
		return 0, 0, err
	}
	return gain, lufs, nil
}

// writeGain 原子保存第一遍测得的确定性增益。
func writeGain(path string, gainDB float64) error {
	tmp := path + ".tmp"
	b := []byte(strconv.FormatFloat(gainDB, 'f', 6, 64))
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return &media.IOError{Op: "write", Path: tmp, Reason: err.Error()}
	}
	if err := os.Rename(tmp, path); err != nil {
		return &media.IOError{Op: "rename", Path: path, Reason: err.Error()}
	}
	return nil
}

// renderPassResumable 从检查点渲染暂存样本为交付 PCM。
// 检查点记录已写的输出字节（OutOffset）与样本帧（EntriesDone），
// 中断或注入写盘失败后再次调用从该偏移继续；无有效检查点时截断重建。
func renderPassResumable(cfg Config, idx *index.Index, stagePath string,
	gainDB float64, cpStore *CheckpointStore, inputDigest string, cp *Checkpoint) error {

	sf, err := os.Open(stagePath)
	if err != nil {
		return &media.IOError{Op: "open", Path: stagePath, Reason: err.Error()}
	}
	defer sf.Close()
	info, err := sf.Stat()
	if err != nil {
		return &media.IOError{Op: "stat", Path: stagePath, Reason: err.Error()}
	}
	totalFrames := info.Size() / 8

	startFrame, outOffset, valid := int64(0), int64(0), false
	if cp != nil && cp.Phase == "render" {
		startFrame, outOffset, valid = int64(cp.EntriesDone), cp.OutOffset, true
	}
	// 定位暂存读取位置（每样本帧左右各 4 字节）。
	if _, err := sf.Seek(startFrame*8, io.SeekStart); err != nil {
		return &media.IOError{Op: "seek", Path: stagePath, Reason: err.Error()}
	}
	// 续跑用 append 接续已有输出；首跑截断重建，保证干净运行确定。
	flag := os.O_RDWR | os.O_CREATE
	if valid {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	out, err := os.OpenFile(cfg.OutputPath, flag, 0o644)
	if err != nil {
		return &media.IOError{Op: "open", Path: cfg.OutputPath, Reason: err.Error()}
	}
	defer out.Close()

	gain := audio.GainLinear(gainDB)
	const block = 512 // 小块强制频繁的中断检查与检查点提交
	leftRaw := make([]byte, block*4)
	rightRaw := make([]byte, block*4)
	pcm := make([]byte, block*audio.TargetChans*2)
	deadline := interruptDeadline(cfg.InterruptAt)
	failBudget := cfg.InjectWriteFailAfter
	done := startFrame
	written := outOffset
	// 中断截止时间基于渲染阶段起点，避免被测量阶段耗时提前耗尽。
	if deadline != nil {
		t := time.Now().Add(cfg.InterruptAt)
		deadline = &t
	}

	for remaining := totalFrames - startFrame; remaining > 0; {
		// 每个块都判断中断，确保短任务也能在截止后立即停下。
		if deadline != nil && time.Now().After(*deadline) {
			return errInterrupted
		}
		n := int(remaining)
		if n > block {
			n = block
		}
		if _, err := io.ReadFull(sf, leftRaw[:n*4]); err != nil {
			return &media.IOError{Op: "read", Path: stagePath, Reason: err.Error()}
		}
		if _, err := io.ReadFull(sf, rightRaw[:n*4]); err != nil {
			return &media.IOError{Op: "read", Path: stagePath, Reason: err.Error()}
		}
		left := bytesF32(leftRaw[:n*4])
		right := bytesF32(rightRaw[:n*4])
		for i := 0; i < n; i++ {
			l := audio.LimitSample(float64(left[i]) * gain)
			r := audio.LimitSample(float64(right[i]) * gain)
			audio.FloatToS16(pcm[i*4:], l)
			audio.FloatToS16(pcm[i*4+2:], r)
		}
		if _, err := out.Write(pcm[:n*4]); err != nil {
			return &media.IOError{Op: "write", Path: cfg.OutputPath, Reason: err.Error()}
		}
		done += int64(n)
		written += int64(n * 4)

		// 周期性原子提交检查点。
		if (done-startFrame)%checkpointEvery == 0 || remaining-int64(n) == 0 {
			if err := cpStore.Save(&Checkpoint{
				InputDigest: inputDigest, ParamDigest: cfg.Params.Digest(),
				EntriesDone: int(done), OutOffset: written,
				GainDB: gainDB, Phase: "render",
			}); err != nil {
				return err
			}
			if failBudget > 0 {
				failBudget--
				if failBudget == 0 {
					return &media.IOError{Op: "write", Path: cfg.OutputPath,
						Reason: "injected write failure"}
				}
			}
		}
		remaining -= int64(n)
	}
	return out.Sync()
}

// bytesF32 零拷贝把字节切片解释为 float32 切片。
func bytesF32(b []byte) []float32 {
	if len(b) < 4 {
		return nil
	}
	return unsafe.Slice((*float32)(unsafe.Pointer(&b[0])), len(b)/4)
}

func interruptDeadline(d time.Duration) *time.Time {
	if d <= 0 {
		return nil
	}
	t := time.Now().Add(d)
	return &t
}

func repLocate(r *Report, seq uint16) {
	if r.LocatedPacket < 0 {
		r.LocatedPacket = int64(seq)
	}
}

func writeStageFrame(w io.Writer, f *astiav.Frame) error {
	b, err := f.Data().Bytes(0)
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return &media.IOError{Op: "write", Reason: err.Error()}
	}
	return nil
}
