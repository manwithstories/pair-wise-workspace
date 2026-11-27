// Package verify 是独立于生产流水线的复核层：只从磁盘读回成品，
// 重新计算像素哈希、块哈希与封装校验和并比对清单，任何不一致都映射到
// 明确的退出码（2 输入无效 / 3 边界或校验和不一致 / 0 通过）。
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"

	"bcast/internal/fault"
	"bcast/internal/model"
	"bcast/internal/render"
	"bcast/internal/resume"
)

// FrameEntry 是清单中的单帧记录（落盘即不可变）。
type FrameEntry struct {
	Index  int    `json:"index"`
	TimeUS int64  `json:"time_us"`
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
}

// TimelineEntry 是时间线清单的单行样本摘要。
type TimelineEntry struct {
	TrackID  uint8  `json:"track_id"`
	Kind     string `json:"kind"`
	Seq      uint64 `json:"seq"`
	PTSUS    int64  `json:"pts_us"`
	Duration int64  `json:"duration_us"`
	Wrap     int    `json:"wrap"`
}

// Report 聚合一次运行的全部可复核指标，JSON 字段固定、顺序确定。
type Report struct {
	Tracks                 int     `json:"tracks"`
	Renditions             int     `json:"renditions"`
	Frames                 int     `json:"frames"`
	FaultLocationErrorPkts int     `json:"fault_location_error_packets"`
	BoundaryMismatches     int     `json:"boundary_mismatches"`
	CrossLayerDeviations   int     `json:"cross_layer_deviations"`
	ResumeMS               int64   `json:"resume_ms"`
	PixelSHA256            string  `json:"pixel_sha256"`
	FullSHA256             string  `json:"full_sha256"`
	ResumedSHA256          string  `json:"resumed_sha256"`
	PackagesIdentical      bool    `json:"packages_identical"`
	BlockVerificationOK    bool    `json:"block_verification_ok"`
	FrameVerificationOK    bool    `json:"frame_verification_ok"`
	DriftPPM               []int64 `json:"drift_ppm"`
}

// AggregatePixelHash 对按索引有序的帧像素哈希再做一层 SHA256，
// 得到整个帧序列的单一 64 位十六进制复核值。
func AggregatePixelHash(refs []model.FrameRef) string {
	sort.Slice(refs, func(i, j int) bool { return refs[i].Index < refs[j].Index })
	h := sha256.New()
	for _, r := range refs {
		h.Write([]byte(r.Sha256))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// VerifyFrames 从磁盘逐帧解码 PNG 并重算像素哈希，与清单逐条比对。
func VerifyFrames(refs []model.FrameRef) (int, error) {
	bad := 0
	for _, ref := range refs {
		got, err := render.RehashPNG(ref.Path)
		if err != nil {
			return bad, err
		}
		if got != ref.Sha256 {
			bad++
		}
	}
	return bad, nil
}

// VerifyPackage 独立复算封装文件：块级哈希链 + 整文件 SHA256。
func VerifyPackage(mpkPath string, j *resume.Journal) (string, error) {
	if err := resume.VerifyBlocks(mpkPath, j); err != nil {
		return "", err
	}
	return resume.FileSHA256(mpkPath)
}

// ComparePackages 比对完整与续跑两个封装的整文件校验和。
func ComparePackages(fullPath, resumedPath string) (full, resumed string, identical bool, err error) {
	full, err = resume.FileSHA256(fullPath)
	if err != nil {
		return "", "", false, err
	}
	resumed, err = resume.FileSHA256(resumedPath)
	if err != nil {
		return "", "", false, err
	}
	return full, resumed, full == resumed, nil
}

// Evaluate 依据核验结果决定退出语义：边界/校验不一致 -> ErrBoundary（码 3）。
func Evaluate(r Report) error {
	if r.BoundaryMismatches != 0 || r.CrossLayerDeviations != 0 {
		return fault.Boundaryf("boundary mismatches=%d cross-layer=%d",
			r.BoundaryMismatches, r.CrossLayerDeviations)
	}
	if !r.PackagesIdentical || !r.BlockVerificationOK || !r.FrameVerificationOK {
		return fault.New(fault.ErrChecksum, "verify", 0, 0, 0, 0,
			"artifact checksum or block verification failed")
	}
	return nil
}

// WriteReport 以确定性 JSON 落盘核验报告。
func WriteReport(path string, r Report) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// WriteTimelineManifest 落盘时间线清单，样本按 (PTS,track,seq) 有序。
func WriteTimelineManifest(path string, entries []TimelineEntry) error {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].PTSUS != entries[j].PTSUS {
			return entries[i].PTSUS < entries[j].PTSUS
		}
		if entries[i].TrackID != entries[j].TrackID {
			return entries[i].TrackID < entries[j].TrackID
		}
		return entries[i].Seq < entries[j].Seq
	})
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// IsInvalidInput 判断错误是否属于参数/输入无效（退出码 2）。
func IsInvalidInput(err error) bool { return errors.Is(err, fault.ErrInvalidInput) }

// IsBoundaryOrChecksum 判断错误是否属于边界/校验不一致（退出码 3）。
func IsBoundaryOrChecksum(err error) bool {
	return errors.Is(err, fault.ErrBoundary) || errors.Is(err, fault.ErrChecksum)
}
