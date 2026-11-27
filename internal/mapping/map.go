// Package mapping 将源机位映射到稳定轨道 ID，并在轨道内做幂等去重与冲突隔离。
package mapping

import (
	"encoding/json"
	"fmt"
	"os"

	"relay/internal/media"
)

// CameraConfig 描述单个机位的时钟偏移与目标轨道。
type CameraConfig struct {
	TrackID       uint32 `json:"track_id"`
	ClockOffsetNS int64  `json:"clock_offset_ns"` // 加到源事件时间上的校正量（纳秒）
}

// Config 是映射配置文件（map.json）的完整结构。
type Config struct {
	Version int                     `json:"version"`
	Cameras map[string]CameraConfig `json:"cameras"`
}

// Mapper 保存解析后的机位表，提供校正与轨道解析。
type Mapper struct {
	cameras map[uint16]CameraConfig
}

// LoadConfig 从路径读取并校验映射配置。
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, media.NewFault(media.StageConfig, 0, 0, 0, "read map: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, media.NewFault(media.StageConfig, 0, 0, 0, "parse map: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate 检查配置自洽：版本、非空、轨道 ID 不重复。
func (c *Config) Validate() error {
	if c.Version != 1 {
		return media.NewFault(media.StageConfig, 0, 0, 0, "unsupported map version %d", c.Version)
	}
	if len(c.Cameras) == 0 {
		return media.NewFault(media.StageConfig, 0, 0, 0, "empty camera map")
	}
	seen := make(map[uint32]bool)
	for key, cam := range c.Cameras {
		var id uint16
		if _, err := fmt.Sscanf(key, "%d", &id); err != nil {
			return media.NewFault(media.StageConfig, 0, id, 0, "bad camera key %q", key)
		}
		if cam.TrackID == 0 {
			return media.NewFault(media.StageConfig, 0, id, 0, "camera %d: track_id must be non-zero", id)
		}
		if seen[cam.TrackID] {
			return media.NewFault(media.StageConfig, 0, id, 0,
				"track %d mapped by multiple cameras", cam.TrackID)
		}
		seen[cam.TrackID] = true
	}
	return nil
}

// NewMapper 把配置编译为快速查询表。
func NewMapper(cfg *Config) (*Mapper, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	m := &Mapper{cameras: make(map[uint16]CameraConfig, len(cfg.Cameras))}
	for key, cam := range cfg.Cameras {
		var id uint16
		fmt.Sscanf(key, "%d", &id)
		m.cameras[id] = cam
	}
	return m, nil
}

// Resolve 校正帧时间并解析轨道；未登记机位返回错误（不得静默丢弃或猜测）。
func (m *Mapper) Resolve(f *media.Frame) (trackID uint32, corrected media.TimeNS, err error) {
	cam, ok := m.cameras[f.CameraID]
	if !ok {
		return 0, 0, media.NewFault(media.StageConfig, f.SourceOffset, f.CameraID, f.Sequence,
			"camera %d not present in map", f.CameraID)
	}
	return cam.TrackID, f.EventNS + media.TimeNS(cam.ClockOffsetNS), nil
}

// MarshalJSON 写出确定性配置（键排序由 encoding/json 对 map 保证）。
func (c *Config) MarshalJSON() ([]byte, error) {
	type alias Config
	return json.MarshalIndent((*alias)(c), "", "  ")
}
