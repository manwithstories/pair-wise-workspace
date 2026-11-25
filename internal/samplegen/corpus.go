package samplegen

import (
	"math/rand"
	"path/filepath"
)

// 三个验收轨道的稳定 ID。
const (
	TrackAAC   = "track-1-aac"
	TrackOpus2 = "track-2-opus"
	TrackOpus3 = "track-3-opus"
)

// Params 是影响时间线和渲染哈希的确定性参数。
type Params struct {
	Generator       string `json:"generator"`
	Seed            int64  `json:"seed"`
	Events          int    `json:"events"`
	Tracks          int    `json:"tracks"`
	FrameWidth      int    `json:"frame_width"`
	FrameHeight     int    `json:"frame_height"`
	FrameInterval   int64  `json:"frame_interval_ticks"`
	AllowedLateness int64  `json:"allowed_lateness_ticks"`
	AACTimescale    uint32 `json:"aac_timescale"`
	AACSampleSize   int    `json:"aac_sample_size"`
	DamageAt        int    `json:"damage_at"`
	ReplayInterval  int    `json:"replay_interval"`
}

// DamageInfo 保存截断 box 样例的预期定位结果。
type DamageInfo struct {
	PacketNo       int    `json:"packet_no"`
	File           string `json:"file"`
	ExpectedOffset int64  `json:"expected_offset"`
	Box            string `json:"box"`
}

// Corpus 是生成后的夹具路径和定位基准。
type Corpus struct {
	Params        Params     `json:"params"`
	FixturesDir   string     `json:"fixtures_dir"`
	AACPath       string     `json:"aac_path"`
	Opus2Path     string     `json:"opus2_path"`
	Opus3Path     string     `json:"opus3_path"`
	ShortRTPPath  string     `json:"short_rtp_path"`
	TruncatedPath string     `json:"truncated_path"`
	Damage        DamageInfo `json:"damage"`
	AACDataOffset uint32     `json:"aac_data_offset"`
}

// DefaultParams 返回验收默认参数。
func DefaultParams(seed int64, events, damageAt int) Params {
	return Params{
		Generator:       "mediaarchive-samplegen-v1",
		Seed:            seed,
		Events:          events,
		Tracks:          3,
		FrameWidth:      256,
		FrameHeight:     144,
		FrameInterval:   96_000,
		AllowedLateness: 48_000,
		AACTimescale:    48_000,
		AACSampleSize:   24,
		DamageAt:        damageAt,
		ReplayInterval:  1000,
	}
}

// Generate 在 out/fixtures 下生成全部源样例。
func Generate(out string, p Params, scenario string) (*Corpus, error) {
	dir := filepath.Join(out, "fixtures")
	perTrack := p.Events / p.Tracks
	trackNos := make([][]int, p.Tracks)
	for no := 1; no <= p.Events; no++ {
		ti := (no - 1) % p.Tracks
		trackNos[ti] = append(trackNos[ti], no)
	}

	rng := rand.New(rand.NewSource(p.Seed + 77))
	aacSamples := make([]AACSampleSpec, perTrack)
	deltas := []uint32{1024, 960, 1024, 1056}
	for i := range aacSamples {
		payload := make([]byte, p.AACSampleSize)
		_, _ = rng.Read(payload)
		aacSamples[i] = AACSampleSpec{Delta: deltas[i%len(deltas)], Payload: payload}
	}

	aacPath := filepath.Join(dir, "track-1-aac.mp4")
	dataOffset, err := WriteAACMP4(aacPath, p.AACTimescale, aacSamples)
	if err != nil {
		return nil, err
	}

	opus2 := generateOpusTrack(trackNos[1], p.Seed, 2)
	opus2Path := filepath.Join(dir, "track-2-opus.rtprec")
	if err := writeRTPRecords(opus2Path, opus2); err != nil {
		return nil, err
	}
	opus3 := generateOpusTrack(trackNos[2], p.Seed, 3)
	opus3Path := filepath.Join(dir, "track-3-opus.rtprec")
	if err := writeRTPRecords(opus3Path, opus3); err != nil {
		return nil, err
	}

	shortPath := filepath.Join(dir, "edge-short-rtp.rtprec")
	if err := writeShortRTPSample(shortPath); err != nil {
		return nil, err
	}

	truncatedPath := filepath.Join(dir, "edge-truncated-box.mp4")
	if _, err := WriteTruncatedBoxSample(truncatedPath, 6001); err != nil {
		return nil, err
	}

	damagePath := truncatedPath
	damagePacket := 6001
	if p.DamageAt > 0 {
		damagePacket = p.DamageAt
	}
	if scenario == "damage" && p.DamageAt > 0 {
		damagePath = filepath.Join(dir, "damage-target-truncated-box.mp4")
	}
	damageOffset, err := WriteTruncatedBoxSample(damagePath, damagePacket)
	if err != nil {
		return nil, err
	}

	return &Corpus{
		Params:        p,
		FixturesDir:   dir,
		AACPath:       aacPath,
		Opus2Path:     opus2Path,
		Opus3Path:     opus3Path,
		ShortRTPPath:  shortPath,
		TruncatedPath: truncatedPath,
		Damage: DamageInfo{
			PacketNo:       damagePacket,
			File:           damagePath,
			ExpectedOffset: damageOffset,
			Box:            "/stts",
		},
		AACDataOffset: dataOffset,
	}, nil
}

// AACGlobalNo 将 AAC stbl 从零开始的样本序号映射回全局事件序号。
func AACGlobalNo(sampleIndex int) int { return 3*sampleIndex + 1 }
