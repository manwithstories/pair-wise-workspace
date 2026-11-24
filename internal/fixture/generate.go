package fixture

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/pion/rtp"

	"bcast/internal/media"
)

// Config is a self-generated sample batch configuration.
type Config struct {
	Case             string  `json:"case"`
	Dir              string  `json:"dir"`
	TrackID          uint32  `json:"track_id"`
	Timescale        uint32  `json:"timescale"`
	FragmentCount    int     `json:"fragment_count"`
	SamplesPerFrag   int     `json:"samples_per_fragment"`
	SampleSize       int     `json:"sample_size"`
	UniformDuration  uint32  `json:"uniform_duration"`
	VFR              bool    `json:"vfr"`
	DriftPPM         float64 `json:"drift_ppm"`
	WrapRTP          bool    `json:"wrap_rtp"`
	FallbackAt       uint32  `json:"fallback_at"`
	DuplicateFrom    uint32  `json:"duplicate_from"`
	DuplicateAs      uint32  `json:"duplicate_as_file"`
	MissingSequence  uint32  `json:"missing_sequence"`
	TruncateSequence uint32  `json:"truncate_sequence"`
	TruncateTail     int64   `json:"truncate_tail_bytes"`
	TargetKBPS       int     `json:"target_kbps"`
}

// CaseConfig returns the named deterministic sample configuration.
func CaseConfig(caseName, dir string) Config {
	c := Config{
		Case:            caseName,
		Dir:             dir,
		TrackID:         1,
		Timescale:       90000,
		FragmentCount:   6,
		SamplesPerFrag:  24,
		SampleSize:      512,
		UniformDuration: 3000,
		TargetKBPS:      6000,
	}
	switch caseName {
	case "clean":
		c.FragmentCount = 24
		c.SamplesPerFrag = 150
		c.SampleSize = 25000
		c.UniformDuration = 3000
	case "truncated":
		c.FragmentCount = 4
		c.SamplesPerFrag = 24
		c.SampleSize = 512
		c.TruncateSequence = 2
		c.TruncateTail = 64
	case "duplicate":
		c.FragmentCount = 6
		c.SamplesPerFrag = 24
		c.SampleSize = 512
		c.DuplicateFrom = 3
		c.DuplicateAs = 7
		c.MissingSequence = 4
	case "drift-wrap-vfr":
		c.FragmentCount = 24
		c.SamplesPerFrag = 48
		c.SampleSize = 1024
		c.VFR = true
		c.DriftPPM = 5000
		c.WrapRTP = true
		c.FallbackAt = 10
	case "commit-interrupt":
		c.FragmentCount = 8
		c.SamplesPerFrag = 24
		c.SampleSize = 1024
	default:
		// Unknown cases still produce a tiny normal batch; CLI validates names.
	}
	return c
}

// Generate creates a named sample batch and returns its configuration.
func Generate(caseName, dir string) (Config, error) {
	cfg := CaseConfig(caseName, dir)
	return cfg, GenerateConfig(cfg)
}

// GenerateConfig creates init.mp4, segments and rtp_anchors.json.
func GenerateConfig(cfg Config) error {
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return media.IO("fixture", cfg.Dir, 0, 0, err)
	}
	initPath := filepath.Join(cfg.Dir, "init.mp4")
	if err := WriteInit(initPath, cfg.TrackID, cfg.Timescale); err != nil {
		return err
	}
	fragDur := fragmentDuration(cfg)
	var anchors []anchorRecord
	for seq := uint32(1); seq <= uint32(cfg.FragmentCount); seq++ {
		durs, cto := timingFor(cfg)
		trueTFDT := uint64((seq - 1)) * uint64(fragDur)
		tfdt := trueTFDT
		if cfg.FallbackAt == seq {
			// Deliberately old base: exercises tfdt fallback repair.
			tfdt = uint64(2) * uint64(fragDur)
		}
		path := segmentPath(cfg.Dir, seq)
		spec := SegmentSpec{
			Path:        path,
			Sequence:    seq,
			TrackID:     cfg.TrackID,
			Timescale:   cfg.Timescale,
			SampleCount: cfg.SamplesPerFrag,
			SampleSize:  cfg.SampleSize,
			Duration:    cfg.UniformDuration,
			Durations:   durs,
			CTO:         cto,
			TFDT:        tfdt,
		}
		if err := WriteSegment(spec); err != nil {
			return err
		}
		rec, err := makeAnchor(cfg, seq, int64(trueTFDT))
		if err != nil {
			return err
		}
		anchors = append(anchors, rec)
	}
	if cfg.DuplicateFrom != 0 && cfg.DuplicateAs != 0 {
		src := segmentPath(cfg.Dir, cfg.DuplicateFrom)
		dst := segmentPath(cfg.Dir, cfg.DuplicateAs)
		if err := copyFile(src, dst); err != nil {
			return err
		}
	}
	if cfg.MissingSequence != 0 {
		path := segmentPath(cfg.Dir, cfg.MissingSequence)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return media.IO("fixture", path, cfg.MissingSequence, 0, err)
		}
	}
	if cfg.TruncateSequence != 0 && cfg.TruncateTail > 0 {
		path := segmentPath(cfg.Dir, cfg.TruncateSequence)
		st, err := os.Stat(path)
		if err != nil {
			return media.IO("fixture", path, cfg.TruncateSequence, 0, err)
		}
		if err := os.Truncate(path, st.Size()-cfg.TruncateTail); err != nil {
			return media.IO("fixture", path, cfg.TruncateSequence, st.Size()-cfg.TruncateTail, err)
		}
	}
	return writeAnchors(cfg, anchors)
}

func segmentPath(dir string, seq uint32) string {
	return filepath.Join(dir, "seg-"+pad5(seq)+".m4s")
}

func pad5(seq uint32) string {
	const digits = "0123456789"
	b := []byte("00000")
	for i := 4; i >= 0; i-- {
		b[i] = digits[seq%10]
		seq /= 10
	}
	return string(b)
}

func fragmentDuration(cfg Config) int64 {
	if !cfg.VFR {
		return int64(cfg.SamplesPerFrag) * int64(cfg.UniformDuration)
	}
	var sum int64
	for i := 0; i < cfg.SamplesPerFrag; i++ {
		if i%2 == 0 {
			sum += 2400
		} else {
			sum += 3600
		}
	}
	return sum
}

func timingFor(cfg Config) ([]uint32, []int32) {
	durs := make([]uint32, cfg.SamplesPerFrag)
	cto := make([]int32, cfg.SamplesPerFrag)
	ctoPattern := []int32{0, 1000, 2000, 1000}
	for i := range durs {
		if cfg.VFR {
			if i%2 == 0 {
				durs[i] = 2400
			} else {
				durs[i] = 3600
			}
		} else {
			durs[i] = cfg.UniformDuration
		}
		cto[i] = ctoPattern[i%len(ctoPattern)]
	}
	return durs, cto
}

type anchorRecord struct {
	TrackID   uint32 `json:"track_id"`
	Fragment  uint32 `json:"fragment"`
	PacketB64 string `json:"packet_b64"`
}

func makeAnchor(cfg Config, seq uint32, tfdt int64) (anchorRecord, error) {
	drift := 1 + cfg.DriftPPM/1_000_000
	base := int64(180_000)
	if cfg.WrapRTP {
		base = int64(math.MaxUint32) - 3*fragmentDuration(cfg)
	}
	unwrapped := base + int64(math.Round(float64(tfdt)*drift))
	pkt := rtp.Packet{Header: rtp.Header{
		Version:        2,
		PayloadType:    96,
		SequenceNumber: uint16(seq),
		Timestamp:      uint32(unwrapped),
		SSRC:           0x42434153,
		Marker:         true,
	}, Payload: []byte("bcast-rtp-anchor")}
	raw, err := pkt.Marshal()
	if err != nil {
		return anchorRecord{}, media.Corrupt("fixture", "rtp_marshal", "", seq, 0, "%v", err)
	}
	return anchorRecord{TrackID: cfg.TrackID, Fragment: seq, PacketB64: base64.StdEncoding.EncodeToString(raw)}, nil
}

func writeAnchors(cfg Config, records []anchorRecord) error {
	env := struct {
		ClockRate uint32         `json:"clock_rate"`
		Records   []anchorRecord `json:"records"`
	}{ClockRate: cfg.Timescale, Records: records}
	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return media.Corrupt("fixture", "anchor_json", cfg.Dir, 0, 0, "%v", err)
	}
	path := filepath.Join(cfg.Dir, "rtp_anchors.json")
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return media.IO("fixture", path, 0, 0, err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return media.IO("fixture", src, 0, 0, err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return media.IO("fixture", dst, 0, 0, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return media.IO("fixture", dst, 0, 0, err)
	}
	return out.Close()
}
