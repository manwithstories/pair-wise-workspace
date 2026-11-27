// Package repack 将核验后的轨道采样重新封装为母版 WAV，
// 损坏缺口按固定策略补零，并同步产出清单与 SHA-256 校验值。
package repack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"

	"bcastqc/internal/model"
)

// ManifestEntry 描述母版中一个被修复的缺口。
type ManifestEntry struct {
	TrackID             int    `json:"track_id"`
	StartSample         int64  `json:"start_sample"`
	EndSample           int64  `json:"end_sample"`
	Kind                string `json:"kind"`
	LocErrorSamples     int64  `json:"loc_error_samples"`
	Strategy            string `json:"repair_strategy"`
	ZeroFilledSamples   int64  `json:"zero_filled_samples"`
}

// Manifest 是与母版 WAV 配套的再封装清单。
type Manifest struct {
	SampleRate       uint32          `json:"sample_rate"`
	BitsPerSample    uint16          `json:"bits_per_sample"`
	NumChans         int             `json:"num_channels"`
	TotalFrames      int64           `json:"total_frames"`
	RecoveredSamples int64           `json:"recovered_samples"`
	Entries          []ManifestEntry `json:"damage_entries"`
	// SHA256 在 Close 后填充为母版 WAV 的摘要。
	SHA256 string `json:"sha256"`
}

// Writer 流式写出多声道母版：内存占用只与写入块大小有关，与总长无关。
type Writer struct {
	outDir    string
	enc       *wav.Encoder
	wavFile   *os.File
	hash      hash.Hash
	manifest  Manifest
	zeroBuf   []int
	frameSize int // 每帧的 int 采样数 = 声道数
	written   int64
}

// NewWriter 创建母版写出器并写好 WAV 头。
func NewWriter(outDir string, sampleRate uint32, bits uint16, chans int, totalFrames int64) (*Writer, error) {
	if chans <= 0 || chans > model.MaxTracks {
		return nil, fmt.Errorf("%w: 声道数 %d 非法", model.ErrInvalidInput, chans)
	}
	if err := model.CheckSampleCount(totalFrames); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("%w: 创建输出目录失败: %v", model.ErrInvalidInput, err)
	}
	path := filepath.Join(outDir, "recovered.wav")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("%w: 创建母版文件失败: %v", model.ErrInvalidInput, err)
	}
	enc := wav.NewEncoder(f, int(sampleRate), int(bits), chans, int(ingestPCMFormat))
	return &Writer{
		outDir:    outDir,
		enc:       enc,
		wavFile:   f,
		hash:      sha256.New(),
		frameSize: chans,
		manifest: Manifest{
			SampleRate:    sampleRate,
			BitsPerSample: bits,
			NumChans:      chans,
			TotalFrames:   totalFrames,
			Entries:       make([]ManifestEntry, 0),
		},
	}, nil
}

// ingestPCMFormat 与 ingest.WaveFormatPCM 保持一致，避免反向依赖。
const ingestPCMFormat = 1

// WriteFrames 写出一段完好的交织采样帧。data 长度必须是 frameSize 的整数倍。
func (w *Writer) WriteFrames(data []int) error {
	if len(data)%w.frameSize != 0 {
		return fmt.Errorf("%w: 采样数据长度 %d 不是声道数 %d 的整数倍",
			model.ErrInvalidInput, len(data), w.frameSize)
	}
	buf := &audio.IntBuffer{
		Data: data,
		Format: &audio.Format{
			NumChannels: w.frameSize,
			SampleRate:  int(w.manifest.SampleRate),
		},
	}
	if err := w.enc.Write(buf); err != nil {
		return fmt.Errorf("写母版帧失败: %w", err)
	}
	w.written += int64(len(data) / w.frameSize)
	return nil
}

// RecordZeroFill 在清单中登记一个补零缺口。实际的零采样由主写入流
// 逐帧交织产出，本方法只负责清单记账，避免重复写入造成帧数翻倍。
func (w *Writer) RecordZeroFill(trackID int, start, end int64, kind string, locErr int64) error {
	frames := end - start
	if frames <= 0 {
		return fmt.Errorf("%w: 补零帧数必须为正，得到 %d", model.ErrInvalidInput, frames)
	}
	w.manifest.Entries = append(w.manifest.Entries, ManifestEntry{
		TrackID:           trackID,
		StartSample:       start,
		EndSample:         end,
		Kind:              kind,
		LocErrorSamples:   locErr,
		Strategy:          "zero-fill",
		ZeroFilledSamples: frames,
	})
	return nil
}

// WriteZeroFill 兼容保留：直接写入纯零帧块，供独立单轨场景使用。
// 多轨交织流水线应改用 RecordZeroFill 记账，由主流统一产出零采样。
func (w *Writer) WriteZeroFill(trackID int, start, end int64, kind string, locErr int64) error {
	frames := end - start
	if frames <= 0 {
		return fmt.Errorf("%w: 补零帧数必须为正，得到 %d", model.ErrInvalidInput, frames)
	}
	if w.zeroBuf == nil || len(w.zeroBuf) < w.frameSize {
		w.zeroBuf = make([]int, w.frameSize)
	}
	const chunkFrames = 4096
	remaining := frames
	for remaining > 0 {
		n := remaining
		if n > chunkFrames {
			n = chunkFrames
		}
		block := make([]int, 0, n*int64(w.frameSize))
		for i := int64(0); i < n; i++ {
			block = append(block, w.zeroBuf...)
		}
		if err := w.WriteFrames(block); err != nil {
			return err
		}
		remaining -= n
	}
	return w.RecordZeroFill(trackID, start, end, kind, locErr)
}

// Close 收尾 WAV、计算 SHA-256 并原子写出清单，返回最终清单。
func (w *Writer) Close() (*Manifest, error) {
	if err := w.enc.Close(); err != nil {
		return nil, fmt.Errorf("关闭 WAV 编码器失败: %w", err)
	}
	path := filepath.Join(w.outDir, "recovered.wav")
	// go-audio 编码器关闭时会对底层 *os.File 执行 Sync；为避免复用 fd
	// 出现 bad file descriptor，这里重新以只读方式打开完整母版计算摘要，
	// 保证与外部 shasum 口径完全一致。
	if err := w.wavFile.Close(); err != nil {
		return nil, fmt.Errorf("关闭母版文件失败: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("回读母版失败: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(w.hash, f); err != nil {
		return nil, fmt.Errorf("计算母版摘要失败: %w", err)
	}
	sum := w.hash.Sum(nil)
	w.manifest.SHA256 = hex.EncodeToString(sum)
	w.manifest.RecoveredSamples = w.written
	if err := w.writeManifestAtomic(); err != nil {
		return nil, err
	}
	return &w.manifest, nil
}

// writeManifestAtomic 以临时文件 + rename 方式落盘清单。
func (w *Writer) writeManifestAtomic() error {
	path := filepath.Join(w.outDir, "manifest.json")
	tmp := path + ".tmp"
	raw, err := json.MarshalIndent(&w.manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: 序列化清单失败: %v", model.ErrCheckpoint, err)
	}
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("%w: 写临时清单失败: %v", model.ErrCheckpoint, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%w: 原子替换清单失败: %v", model.ErrCheckpoint, err)
	}
	return nil
}

// WrittenFrames 返回已写出的采样帧数。
func (w *Writer) WrittenFrames() int64 { return w.written }

// GapsToEntries 将时间线缺口转换为清单条目视图（供校验/展示复用）。
func GapsToEntries(gaps []model.Gap) []ManifestEntry {
	out := make([]ManifestEntry, 0, len(gaps))
	for _, g := range gaps {
		out = append(out, ManifestEntry{
			TrackID:           g.TrackID,
			StartSample:       g.Start,
			EndSample:         g.End,
			Kind:              g.Kind,
			LocErrorSamples:   g.LocError,
			Strategy:          "zero-fill",
			ZeroFilledSamples: g.Length(),
		})
	}
	return out
}
