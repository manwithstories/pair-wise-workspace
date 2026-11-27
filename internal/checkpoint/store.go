// Package checkpoint 管理恢复点：输入偏移、每轨水位、已提交输出长度与去重账本。
// 写入采用临时文件 + fsync + 原子改名；加载时自校验并核对输入/映射指纹。
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"relay/internal/media"
	"relay/internal/timeline"
)

// Version 是检查点格式版本。
const Version = 1

// QuarantineRec 持久化隔离记录（恢复后仍保持隔离，不重复处理）。
type QuarantineRec struct {
	TrackID  uint32 `json:"track_id"`
	CameraID uint16 `json:"camera_id"`
	Sequence uint64 `json:"sequence"`
	EventNS  int64  `json:"event_ns"`
	Reason   string `json:"reason"`
	Detail   string `json:"detail"`
}

// Data 是检查点的全部可恢复状态。
type Data struct {
	Version int `json:"version"`

	InputPath   string `json:"input_path"`
	InputSize   int64  `json:"input_size"`
	InputSHA256 string `json:"input_sha256"`
	MapPath     string `json:"map_path"`
	MapSHA256   string `json:"map_sha256"`

	OutputPath   string `json:"output_path"`
	OutputFormat string `json:"output_format"`

	InputOffset        int64 `json:"input_offset"`
	CommittedOutputLen int64 `json:"committed_output_len"`
	FramesConsumed     int64 `json:"frames_consumed"`
	FramesReleased     int64 `json:"frames_released"`
	Duplicates         int64 `json:"duplicates"`
	Quarantined        int64 `json:"quarantined"`

	Watermarks map[uint32]int64             `json:"watermarks"`
	Dedup      map[uint32]map[uint64]string `json:"dedup"`
	Quarantine []QuarantineRec              `json:"quarantine"`
	GraceNS    int64                        `json:"grace_ns"`
}

// envelope 是落盘包装：载荷 + 载荷的 SHA-256，损坏/半截文件可被检出。
type envelope struct {
	Payload string `json:"payload"` // hex 编码的 JSON
	SHA256  string `json:"sha256"`
}

// Save 原子写入检查点；目标目录与输出文件同盘时 rename 为原子操作。
func Save(path string, d *Data) error {
	d.Version = Version
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return media.NewFault(media.StageCheckpoint, 0, 0, 0, "marshal: %v", err)
	}
	sum := sha256.Sum256(raw)
	env := envelope{Payload: hex.EncodeToString(raw), SHA256: hex.EncodeToString(sum[:])}
	out, err := json.MarshalIndent(&env, "", "  ")
	if err != nil {
		return media.NewFault(media.StageCheckpoint, 0, 0, 0, "marshal envelope: %v", err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return media.NewFault(media.StageCheckpoint, 0, 0, 0, "open temp: %v", err)
	}
	if _, err := f.Write(out); err != nil {
		f.Close()
		os.Remove(tmp)
		return media.NewFault(media.StageCheckpoint, 0, 0, 0, "write temp: %v", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return media.NewFault(media.StageCheckpoint, 0, 0, 0, "fsync: %v", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return media.NewFault(media.StageCheckpoint, 0, 0, 0, "close: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return media.NewFault(media.StageCheckpoint, 0, 0, 0, "rename: %v", err)
	}
	return nil
}

// Load 读取并自校验检查点；摘要不符返回 checkpoint 阶段错误（退出码 4）。
func Load(path string) (*Data, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, media.NewFault(media.StageCheckpoint, 0, 0, 0, "read: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, media.NewFault(media.StageCheckpoint, 0, 0, 0, "parse envelope: %v", err)
	}
	payload, err := hex.DecodeString(env.Payload)
	if err != nil {
		return nil, media.NewFault(media.StageCheckpoint, 0, 0, 0, "decode payload: %v", err)
	}
	sum := sha256.Sum256(payload)
	if hex.EncodeToString(sum[:]) != env.SHA256 {
		return nil, media.NewFault(media.StageCheckpoint, 0, 0, 0, "checkpoint self-checksum mismatch")
	}
	var d Data
	if err := json.Unmarshal(payload, &d); err != nil {
		return nil, media.NewFault(media.StageCheckpoint, 0, 0, 0, "parse payload: %v", err)
	}
	if d.Version != Version {
		return nil, media.NewFault(media.StageCheckpoint, 0, 0, 0, "unsupported checkpoint version %d", d.Version)
	}
	return &d, nil
}

// FileSHA256 计算文件摘要（十六进制），用于输入/映射指纹核对。
func FileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// VerifyFingerprints 核对当前输入与映射是否与检查点一致；不一致即停止。
func VerifyFingerprints(d *Data, inputSHA, mapSHA string, inputSize int64) error {
	if d.InputSHA256 != inputSHA || d.InputSize != inputSize {
		return media.NewFault(media.StageCheckpoint, 0, 0, 0,
			"input changed since checkpoint: want %s/%d, got %s/%d",
			d.InputSHA256, d.InputSize, inputSHA, inputSize)
	}
	if d.MapSHA256 != mapSHA {
		return media.NewFault(media.StageCheckpoint, 0, 0, 0, "map config changed since checkpoint")
	}
	return nil
}

// TruncateOutput 按已提交长度截断输出尾部，丢弃未提交的部分写入。
func TruncateOutput(path string, committed int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return media.NewFault(media.StageCheckpoint, 0, 0, 0, "stat output: %v", err)
	}
	if info.Size() < committed {
		return media.NewFault(media.StageCheckpoint, 0, 0, 0,
			"output %d bytes shorter than committed %d", info.Size(), committed)
	}
	if err := os.Truncate(path, committed); err != nil {
		return media.NewFault(media.StageCheckpoint, 0, 0, 0, "truncate: %v", err)
	}
	return nil
}

// FromTimeline 把时间线状态与隔离列表转换为可持久化形式。
func FromTimeline(st timeline.State, quar []*timeline.Quarantine) (map[uint32]int64, []QuarantineRec) {
	wm := make(map[uint32]int64, len(st.MaxEvent))
	for t, mx := range st.MaxEvent {
		wm[t] = mx - st.GraceNS // 存水位而非最大事件时间
	}
	recs := make([]QuarantineRec, 0, len(quar))
	for _, q := range quar {
		recs = append(recs, QuarantineRec{
			TrackID: q.Env.TrackID, CameraID: q.Env.Frame.CameraID,
			Sequence: q.Env.Frame.Sequence, EventNS: int64(q.Env.Frame.EventNS),
			Reason: q.Reason, Detail: q.Detail,
		})
	}
	return wm, recs
}

// String 便于日志输出。
func (d *Data) String() string {
	return fmt.Sprintf("offset=%d outlen=%d consumed=%d released=%d",
		d.InputOffset, d.CommittedOutputLen, d.FramesConsumed, d.FramesReleased)
}
