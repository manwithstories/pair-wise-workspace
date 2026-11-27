// Package remux 把统一帧无损转封装为 MFS/1 或 MTA/1，
// 同时生成逐帧来源清单并支持独立重读校验。
package remux

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"relay/internal/media"
	"relay/internal/timeline"
)

// Format 标识输出容器格式。
type Format string

// 支持的输出格式。
const (
	FormatMFS Format = "mfs"
	FormatMTA Format = "mta"
)

// ParseFormat 从文件扩展名或显式名称解析格式。
func ParseFormat(name string) (Format, error) {
	// 若包含扩展名（如 out.mta），先截取扩展名部分。
	if i := lastDot(name); i >= 0 {
		name = name[i:]
	}
	switch name {
	case "mfs", ".mfs":
		return FormatMFS, nil
	case "mta", ".mta", "tar", ".tar":
		return FormatMTA, nil
	default:
		return "", media.NewFault(media.StageConfig, 0, 0, 0, "unknown output format %q", name)
	}
}

// lastDot 返回最后一个 '.' 的位置，无则 -1。
func lastDot(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

// ManifestEntry 是逐帧来源清单的一行，按写出顺序排列。
type ManifestEntry struct {
	Index         int    `json:"index"`
	TrackID       uint32 `json:"track_id"`
	CameraID      uint16 `json:"camera_id"`
	Sequence      uint64 `json:"sequence"`
	EventNS       int64  `json:"event_ns"`
	Codec         string `json:"codec"`
	Width         uint32 `json:"width"`
	Height        uint32 `json:"height"`
	PayloadSHA256 string `json:"payload_sha256"`
	RecordCRC32C  uint32 `json:"record_crc32c"`
	SourceName    string `json:"source_name"`
	SourceOffset  int64  `json:"source_offset"`
	OutputOffset  int64  `json:"output_offset"`
	RecordBytes   int    `json:"record_bytes"`
}

// Manifest 是清单文件的根结构。
type Manifest struct {
	Format  string          `json:"format"`
	Entries []ManifestEntry `json:"entries"`
}

// TarMemberName 返回 MTA/1 成员的确定性命名（机位+序号）。
func TarMemberName(cam uint16, seq uint64) string {
	return fmt.Sprintf("cam-%04x-seq-%016x.rec", cam, seq)
}

// Writer 顺序写出容器与清单；载荷字节原样保留，不做重编码。
type Writer struct {
	f        *os.File
	format   Format
	tw       *tar.Writer // 仅 MTA 使用
	index    int
	bytes    int64
	manifest Manifest
}

// NewWriter 创建/截断输出文件；MTA 模式同时初始化 tar 写入器。
func NewWriter(path string, format Format) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, media.NewFault(media.StageConfig, 0, 0, 0, "open output: %v", err)
	}
	w := &Writer{f: f, format: format, manifest: Manifest{Format: string(format)}}
	if format == FormatMTA {
		w.tw = tar.NewWriter(f)
	}
	return w, nil
}

// WriteFrame 写出一帧并登记清单；返回该记录在输出中的字节偏移。
func (w *Writer) WriteFrame(env *timeline.Envelope) (int64, error) {
	fr := env.Frame
	rec := media.EncodeRecord(fr) // 重新计算线格式校验，载荷字节不变
	outOff := w.bytes

	if w.format == FormatMFS {
		var prefix [media.PrefixLen]byte
		binary.LittleEndian.PutUint32(prefix[:], uint32(len(rec)))
		if _, err := w.f.Write(prefix[:]); err != nil {
			return 0, media.NewFault(media.StageConfig, outOff, fr.CameraID, fr.Sequence, "write prefix: %v", err)
		}
		if _, err := w.f.Write(rec); err != nil {
			return 0, media.NewFault(media.StageConfig, outOff, fr.CameraID, fr.Sequence, "write record: %v", err)
		}
		w.bytes += int64(media.PrefixLen + len(rec))
	} else {
		hdr := &tar.Header{
			Name: TarMemberName(fr.CameraID, fr.Sequence),
			Mode: 0o644, Size: int64(len(rec)),
		}
		if err := w.tw.WriteHeader(hdr); err != nil {
			return 0, media.NewFault(media.StageTar, outOff, fr.CameraID, fr.Sequence, "tar header: %v", err)
		}
		if _, err := w.tw.Write(rec); err != nil {
			return 0, media.NewFault(media.StageTar, outOff, fr.CameraID, fr.Sequence, "tar write: %v", err)
		}
		// tar 成员按 512 字节分块，已提交长度按归档实际占用计算。
		w.bytes += 512 + (int64(len(rec))+511)/512*512
	}

	sum := sha256.Sum256(fr.Payload)
	w.manifest.Entries = append(w.manifest.Entries, ManifestEntry{
		Index: w.index, TrackID: env.TrackID, CameraID: fr.CameraID,
		Sequence: fr.Sequence, EventNS: int64(fr.EventNS),
		Codec: media.CodecName(fr.Codec), Width: fr.Width, Height: fr.Height,
		PayloadSHA256: hex.EncodeToString(sum[:]), RecordCRC32C: fr.CRC32C,
		SourceName: fr.SourceName, SourceOffset: fr.SourceOffset,
		OutputOffset: outOff, RecordBytes: len(rec),
	})
	w.index++
	return outOff, nil
}

// WrittenBytes 返回已提交输出长度（检查点用）。
func (w *Writer) WrittenBytes() int64 { return w.bytes }

// Frames 返回已写出帧数。
func (w *Writer) Frames() int { return w.index }

// Close 收尾容器并把清单写到 manifestPath（确定性缩进 JSON）。
func (w *Writer) Close(manifestPath string) error {
	if w.format == FormatMTA {
		if err := w.tw.Close(); err != nil {
			return media.NewFault(media.StageTar, w.bytes, 0, 0, "tar close: %v", err)
		}
		// 结束双零块占 1024 字节，计入已提交长度。
		w.bytes += 1024
	}
	if err := w.f.Sync(); err != nil {
		return media.NewFault(media.StageConfig, w.bytes, 0, 0, "fsync output: %v", err)
	}
	if err := w.f.Close(); err != nil {
		return media.NewFault(media.StageConfig, w.bytes, 0, 0, "close output: %v", err)
	}
	raw, err := json.MarshalIndent(&w.manifest, "", "  ")
	if err != nil {
		return media.NewFault(media.StageConfig, 0, 0, 0, "marshal manifest: %v", err)
	}
	if err := os.WriteFile(manifestPath, raw, 0o644); err != nil {
		return media.NewFault(media.StageConfig, 0, 0, 0, "write manifest: %v", err)
	}
	return nil
}

// ManifestDigest 计算清单文件的 SHA-256（清单摘要）。
func ManifestDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
