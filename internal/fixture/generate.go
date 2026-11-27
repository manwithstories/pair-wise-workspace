package fixture

import (
	"encoding/binary"
	"os"
	"path/filepath"

	"relay/internal/mapping"
	"relay/internal/media"
)

// 故障标签，与 CLI --faults 取值一一对应。
const (
	FaultCRC       = "crc"
	FaultTruncated = "truncated"
	FaultLate      = "late"
	FaultDuplicate = "duplicate"
	FaultSlow      = "slow"
	FaultInterrupt = "interrupt"
)

// Plan 描述一次夹具生成的输入。
type Plan struct {
	Dir    string
	Frames int      // 基础帧总数（按机位均摊）
	Faults []string // 启用的故障标签
}

// Outputs 记录实际写出的文件路径。
type Outputs struct {
	Map       string
	Live      string
	BadCRC    string
	Truncated string
	Conflict  string
	Beyond    string
	Slow      string
}

// writeMFSFile 把帧编码为 MFS/1 写入 path。
func writeMFSFile(path string, frames []*media.Frame) error {
	var buf []byte
	for _, f := range frames {
		rec := media.EncodeRecord(f)
		var p [media.PrefixLen]byte
		binary.LittleEndian.PutUint32(p[:], uint32(len(rec)))
		buf = append(buf, p[:]...)
		buf = append(buf, rec...)
	}
	return os.WriteFile(path, buf, 0o644)
}

// writeMap 生成与各机位时钟偏移相反的校正配置。
func writeMap(path string) error {
	cfg := &mapping.Config{Version: 1, Cameras: map[string]mapping.CameraConfig{}}
	for cam := uint16(1); cam <= CameraCount; cam++ {
		cfg.Cameras[itoa(cam)] = mapping.CameraConfig{
			TrackID:       100 + uint32(cam),
			ClockOffsetNS: -int64(cam) * cameraBaseNS,
		}
	}
	raw, err := cfg.MarshalJSON()
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

// Generate 按 Plan 生成全部样例；输出路径写入 *Outputs。
func Generate(p Plan) (*Outputs, error) {
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		return nil, err
	}
	if p.Frames <= 0 {
		p.Frames = CameraCount
	}
	perCam := p.Frames / CameraCount
	if perCam < 2 {
		perCam = 2
	}
	enabled := map[string]bool{}
	for _, f := range p.Faults {
		enabled[f] = true
	}

	out := &Outputs{
		Map:  filepath.Join(p.Dir, "map.json"),
		Live: filepath.Join(p.Dir, "live.mfs"),
	}
	if err := writeMap(out.Map); err != nil {
		return nil, err
	}

	live := buildLiveSet(perCam)
	if err := writeMFSFile(out.Live, live); err != nil {
		return nil, err
	}

	if enabled[FaultCRC] {
		out.BadCRC = filepath.Join(p.Dir, "bad-crc.mfs")
		if err := writeBadCRC(out.BadCRC, perCam); err != nil {
			return nil, err
		}
	}
	if enabled[FaultTruncated] {
		out.Truncated = filepath.Join(p.Dir, "truncated.mfs")
		if err := writeTruncated(out.Truncated, perCam); err != nil {
			return nil, err
		}
	}
	if enabled[FaultDuplicate] {
		out.Conflict = filepath.Join(p.Dir, "conflict.mfs")
		if err := writeConflict(out.Conflict); err != nil {
			return nil, err
		}
	}
	if enabled[FaultLate] {
		out.Beyond = filepath.Join(p.Dir, "beyond-window.mfs")
		if err := writeBeyondWindow(out.Beyond); err != nil {
			return nil, err
		}
	}
	if enabled[FaultSlow] {
		out.Slow = filepath.Join(p.Dir, "slow.mfs")
		if err := writeSlow(out.Slow); err != nil {
			return nil, err
		}
	}
	// interrupt 不产生独立数据文件：由 run --interrupt-after 在运行期模拟。
	_ = enabled[FaultInterrupt]
	return out, nil
}

// itoa 避免引入 strconv 造成的小工具分散。
func itoa(n uint16) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
