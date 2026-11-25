// Package manifest writes the quality report that proves a remux run kept
// every packet and stayed within the archive bit-rate budget. All values are
// derived from measured output, never assumed.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"mediaarchive/internal/model"
)

// Damage is one recovered event rendered into the report.
type Damage struct {
	Stage      string `json:"stage"`
	TrackID    int32  `json:"track_id"`
	Offset     int64  `json:"offset"`
	SampleSeq  uint32 `json:"sample_seq"`
	Reason     string `json:"reason"`
	Recovered  bool   `json:"recovered"`
}

// Manifest is the full quality document.
type Manifest struct {
	Version            int      `json:"version"`
	InputPath          string   `json:"input_path"`
	InputSHA256        string   `json:"input_sha256"`
	OutputPath         string   `json:"output_path"`
	OutputSHA256       string   `json:"output_sha256"`
	TargetBPS          int64    `json:"target_bps"`
	ActualBPS          int64    `json:"actual_bps"`
	DeviationPct       float64  `json:"deviation_pct"`
	SampleSHA256Match  float64  `json:"sample_sha256_match"`
	DroppedSamples     int      `json:"dropped_samples"`
	MaxErrorNS         int64    `json:"manifest_max_error_ns"`
	DurationSec        float64  `json:"duration_sec"`
	PacketsIn          int      `json:"packets_in"`
	PacketsOut         int      `json:"packets_out"`
	Damages            []Damage `json:"damages"`
	Recovered          bool     `json:"recovered"`
	Tracks             []int    `json:"tracks"`
	PacketHashes       map[string][]string `json:"packet_hashes"`
}

// Quality inputs collected while the pipeline runs.
type Quality struct {
	TargetBPS   int64
	PayloadBytes int64
	DurationNS  int64
	PacketsIn   int
	PacketsOut  int
	HashMatches int
	HashTotal   int
	Dropped     int
	MaxErrorNS  int64
	Damages     []Damage
	Recovered   bool
	Tracks      []int
	Hashes      map[uint32][][32]byte
}

// Build computes all derived metrics. Bit rate uses payload byte count over
// measured timeline duration; deviation is absolute percent of target.
func Build(in, out, inSHA string, q Quality) (*Manifest, error) {
	outSHA, err := hashFile(out)
	if err != nil {
		return nil, err
	}
	var actual int64
	if q.DurationNS > 0 {
		actual = q.PayloadBytes * 8 * 1_000_000_000 / q.DurationNS
	}
	var dev float64
	if q.TargetBPS > 0 {
		dev = float64(abs64(actual-q.TargetBPS)) * 100 / float64(q.TargetBPS)
	}
	match := 0.0
	if q.HashTotal > 0 {
		match = float64(q.HashMatches) * 100 / float64(q.HashTotal)
	}
	ph := map[string][]string{}
	for id, hs := range q.Hashes {
		key := itoa(int(id))
		for _, h := range hs {
			ph[key] = append(ph[key], hex.EncodeToString(h[:]))
		}
	}
	return &Manifest{
		Version:           1,
		InputPath:         in,
		InputSHA256:       inSHA,
		OutputPath:        out,
		OutputSHA256:      outSHA,
		TargetBPS:         q.TargetBPS,
		ActualBPS:         actual,
		DeviationPct:      round2(dev),
		SampleSHA256Match: round2(match),
		DroppedSamples:    q.Dropped,
		MaxErrorNS:        q.MaxErrorNS,
		DurationSec:       round4(float64(q.DurationNS) / 1e9),
		PacketsIn:         q.PacketsIn,
		PacketsOut:        q.PacketsOut,
		Damages:           q.Damages,
		Recovered:         q.Recovered,
		Tracks:            q.Tracks,
		PacketHashes:      ph,
	}, nil
}

// Save writes to a temp file and atomically replaces the destination.
func (m *Manifest) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return model.NewError(model.StageManifest, "cannot create manifest dir").Wrap(err)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return model.NewError(model.StageManifest, "encode failed").Wrap(err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".manifest-*")
	if err != nil {
		return model.NewError(model.StageManifest, "temp create failed").Wrap(err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return model.NewError(model.StageManifest, "write failed").Wrap(err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return model.NewError(model.StageManifest, "fsync failed").Wrap(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return model.NewError(model.StageManifest, "close failed").Wrap(err)
	}
	if err := os.Rename(name, path); err != nil {
		return model.NewError(model.StageManifest, "atomic rename failed").Wrap(err)
	}
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", model.NewError(model.StageManifest, "cannot open output for hashing").Wrap(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := copyAll(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func copyAll(dst io.Writer, src io.Reader) (int64, error) {
	n, err := io.Copy(dst, src)
	if err != nil {
		return 0, model.NewError(model.StageManifest, "output hash read failed").Wrap(err)
	}
	return n, nil
}

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }
func round4(v float64) float64 { return float64(int64(v*10000+0.5)) / 10000 }

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
