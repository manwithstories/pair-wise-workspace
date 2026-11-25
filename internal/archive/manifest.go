package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"

	"mediaarchive/internal/fault"
	"mediaarchive/internal/ingest"
)

// ManifestVersion 是溯源清单格式版本。
const ManifestVersion = 1

// SourceProvenance 记录一个帧区间内使用的源包及其精确源偏移。
type SourceProvenance struct {
	TrackID       string   `json:"track_id"`
	PacketNo      int      `json:"packet_no"`
	Identity      string   `json:"identity"`
	ExtSeq        int64    `json:"ext_seq"`
	Codec         string   `json:"codec"`
	ContentSHA256 string   `json:"content_sha256"`
	SourcePath    string   `json:"source_path"`
	Container     string   `json:"container"`
	Offset        int64    `json:"offset"`
	RecordOffset  int64    `json:"record_offset,omitempty"`
	Length        int      `json:"length"`
	Box           string   `json:"box,omitempty"`
	SampleIndex   int      `json:"sample_index,omitempty"`
	StartTicks    int64    `json:"start_ticks"`
	DurationTicks int64    `json:"duration_ticks"`
	Flags         []string `json:"flags,omitempty"`
}

// FrameManifest 是单帧 PNG 的来源清单。
type FrameManifest struct {
	Frame              int                `json:"frame"`
	PTSTicks           int64              `json:"pts_ticks"`
	IntervalTicks      int64              `json:"interval_ticks"`
	PNGPath            string             `json:"png_path"`
	PNGSHA256          string             `json:"png_sha256"`
	SourceManifestPath string             `json:"source_manifest_path,omitempty"`
	Sources            []SourceProvenance `json:"sources"`
	HoleTicks          []fault.Hole       `json:"holes,omitempty"`
}

// Manifest 是所有帧、源包、空洞、冲突和迟到影响的确定性溯源清单。
type Manifest struct {
	Version            int                `json:"version"`
	Profile            string             `json:"profile"`
	Seed               int64              `json:"seed"`
	Events             int                `json:"events"`
	Tracks             int                `json:"tracks"`
	FrameIntervalTicks int64              `json:"frame_interval_ticks"`
	ParamsHash         string             `json:"params_hash"`
	StateHash          string             `json:"state_hash"`
	Frames             []FrameManifest    `json:"frames"`
	Holes              []fault.Hole       `json:"holes"`
	Conflicts          []fault.Conflict   `json:"conflicts"`
	LateEvents         []fault.LateImpact `json:"late_events"`
}

// ProvenanceFromPacket 从归一包构造溯源记录。
func ProvenanceFromPacket(p ingest.Packet, flags ...string) SourceProvenance {
	return SourceProvenance{
		TrackID:       p.TrackID,
		PacketNo:      p.No,
		Identity:      p.Identity(),
		ExtSeq:        p.ExtSeq,
		Codec:         p.Codec,
		ContentSHA256: p.Hash,
		SourcePath:    p.Source.Path,
		Container:     p.Source.Container,
		Offset:        p.Source.Offset,
		RecordOffset:  p.Source.RecordOffset,
		Length:        p.Source.Length,
		Box:           p.Source.Box,
		SampleIndex:   p.Source.SampleIndex,
		StartTicks:    int64(p.Start),
		DurationTicks: int64(p.Duration),
		Flags:         flags,
	}
}

// HashBytes 返回原始字节的 SHA256。
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// WriteJSON 以稳定缩进 JSON 原子写入并返回字节哈希。
func WriteJSON(path string, v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	b = append(b, '\n')
	if err := AtomicWriteFile(path, b, 0o644); err != nil {
		return "", err
	}
	return HashBytes(b), nil
}

// WriteFrameSourceManifest 写单帧来源清单。
func WriteFrameSourceManifest(dir string, fm FrameManifest) (string, string, error) {
	name := filepath.Join(dir, "manifests", frameName(fm.Frame, ".sources.json"))
	h, err := WriteJSON(name, fm)
	if err != nil {
		return "", "", err
	}
	return filepath.ToSlash(filepath.Join("manifests", frameName(fm.Frame, ".sources.json"))), h, nil
}

// WriteManifest 写顶层清单并返回文件字节 SHA256。
func WriteManifest(path string, m Manifest) (string, error) {
	h, err := WriteJSON(path, m)
	if err != nil {
		return "", err
	}
	return h, nil
}

func frameName(i int, ext string) string {
	const width = 6
	s := intToPadded(i, width)
	return "frame-" + s + ext
}

func intToPadded(v, width int) string {
	neg := v < 0
	if neg {
		v = -v
	}
	buf := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	for i := 0; i < width; i++ {
		if buf[i] != '0' {
			if neg {
				return "-" + string(buf[i:])
			}
			return string(buf[i:])
		}
	}
	return "0"
}
