// Package report builds and serializes the stable JSON job report.
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"rcx/internal/quality"
)

const SchemaVersion = 1

type SegmentReport struct {
	ID     int     `json:"id"`
	Bits   uint64  `json:"bits"`
	BPS    float64 `json:"bps"`
	QP     int     `json:"qp"`
	PSNRDB float64 `json:"psnr_db"`
	CRC    uint32  `json:"crc"`
}

type Report struct {
	SchemaVersion    int             `json:"schema_version"`
	TargetBPS        int64           `json:"target_bps"`
	TotalTargetBits  uint64          `json:"total_target_bits"`
	DurationSec      float64         `json:"duration_sec"`
	TotalBits        uint64          `json:"total_bits"`
	ActualBPS        float64         `json:"actual_bps"`
	DeviationPct     float64         `json:"deviation_pct"`
	PSNRMeanDB       float64         `json:"psnr_mean_db"`
	PSNRSegmentStdDB float64         `json:"psnr_segment_std_db"`
	SegmentCount     int             `json:"segment_count"`
	Segments         []SegmentReport `json:"segments"`
}

func Build(targetBPS int64, targetBits uint64, durationSec float64, segments []SegmentReport) Report {
	segs := append([]SegmentReport(nil), segments...)
	sort.Slice(segs, func(i, j int) bool { return segs[i].ID < segs[j].ID })
	var totalBits uint64
	values := make([]float64, 0, len(segs))
	for _, s := range segs {
		totalBits += s.Bits
		values = append(values, s.PSNRDB)
	}
	actual := 0.0
	deviation := 0.0
	if durationSec > 0 {
		actual = float64(totalBits) / durationSec
	}
	if targetBPS > 0 {
		deviation = (actual - float64(targetBPS)) * 100 / float64(targetBPS)
	}
	return Report{
		SchemaVersion:    SchemaVersion,
		TargetBPS:        targetBPS,
		TotalTargetBits:  targetBits,
		DurationSec:      durationSec,
		TotalBits:        totalBits,
		ActualBPS:        actual,
		DeviationPct:     deviation,
		PSNRMeanDB:       quality.Mean(values),
		PSNRSegmentStdDB: quality.StdDev(values),
		SegmentCount:     len(segs),
		Segments:         segs,
	}
}

func (r Report) Encode() ([]byte, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func Write(path string, r Report) error {
	data, err := r.Encode()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".rcx-report-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func Load(path string) (Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Report{}, err
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return Report{}, fmt.Errorf("parse report: %w", err)
	}
	if r.SchemaVersion != SchemaVersion {
		return Report{}, fmt.Errorf("unsupported report schema %d", r.SchemaVersion)
	}
	if len(r.Segments) == 0 || r.SegmentCount != len(r.Segments) {
		return Report{}, fmt.Errorf("report has invalid segment list")
	}
	return r, nil
}
