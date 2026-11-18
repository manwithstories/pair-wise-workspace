package main

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// 样例时钟参数：音频钟相对视频参考钟满足 a = αv + β（归零读数口径）。
const (
	genBetaNS   int64   = 80_000_000 // 真值起始偏差 80ms
	genDriftPPM float64 = 240        // 真值漂移 240ppm
	genAnomaly  float64 = 0.10       // 10% 锚点注入异常
	anchorEvery float64 = 2.0        // 每 2 秒一个同步锚点（视频参考时间）
	segSeconds  float64 = 10.0       // 帧率每 10 秒交替 25/50fps
	guardMS     float64 = 0.008      // 锚点两侧保护带（秒），保证异常后仍单调
)

// GenConfig 控制样例生成；时钟参数取包内常量真值，保证验收口径唯一。
type GenConfig struct {
	Seed    int64
	Seconds int
	OutPath string
}

// GenTruth 回传生成侧真值与统计，供终端输出与测试断言。
type GenTruth struct {
	BetaNS     int64
	DriftPPM   float64
	VideoCount int
	AudioCount int
	Anchors    int
	BadAnchors int
	VideoWraps int
	AudioWraps int
}

// lcg 是确定性线性同余随机源（MMIX 参数），保证同 seed 逐字节复现。
type lcg struct{ state uint64 }

func newLCG(seed int64) *lcg { return &lcg{state: uint64(seed)} }

func (g *lcg) next() uint64 {
	g.state = g.state*6364136223846793005 + 1442695040888963407
	return g.state
}

// float 返回 [0,1) 均匀浮点。
func (g *lcg) float() float64 {
	return float64(g.next()>>11) / float64(uint64(1)<<53)
}

const (
	videoTB    = 1_000_000 // 视频 1 tick = 1µs
	audioTB    = 768_000   // 音频 1 tick = 1/768000 s（24kHz×32 音频钟）
	period32   = int64(1) << 32
	audioStepS = 0.01 // 音频普通记录网格 10ms
)

type audioPoint struct {
	ns   float64 // 归零后的精确读数（纳秒）
	sync *string // 非空表示锚点
}

// Generate 写出符合输入契约的 JSONL 样例。
//
// 物理模型（归零读数口径）：视频参考钟 v 从物理 τ=0 起算；音频零读数
// 对应视频零点之前 β 对应的物理时刻，因此 a = αv + β 在 v=0 处恰为 β。
// 这保证两轨各自“首点归零”后 β 仍可被拟合恢复（首条音频记录读数为 0，
// 视频在 a=β 处才有最早配对，负/正时间均保留，不做任何补帧）。
// 含 32 位计数器回绕、25/50fps 交替可变帧率、每 2 秒锚点、10% 锚点异常。
func Generate(cfg GenConfig) (*GenTruth, error) {
	if cfg.Seconds <= 0 {
		return nil, errInvalidGenArg("seconds must be positive")
	}
	alpha := 1.0 + genDriftPPM/1e6
	beta := float64(genBetaNS)
	rng := newLCG(cfg.Seed)
	truth := &GenTruth{BetaNS: genBetaNS, DriftPPM: genDriftPPM}

	// 锚点物理（视频参考）时间：0,2,4,... 严格小于 Seconds（视频帧不含右端点）。
	var anchorV []float64
	anchorIDByV := make(map[float64]string)
	nAnchor := int(math.Ceil(float64(cfg.Seconds) / anchorEvery))
	for i := 0; i < nAnchor; i++ {
		v := float64(i) * anchorEvery
		anchorV = append(anchorV, v)
		anchorIDByV[v] = anchorID(v)
		truth.Anchors++
	}

	// ---- 音频轨读数点：精确锚点 + 10ms 网格普通点（避开锚点保护带）----
	var pts []audioPoint
	// 锚点（精确模型值 a=αv+β，纳秒口径），10% 注入 ±3–6ms 异常。
	for i, v := range anchorV {
		a := alpha*v*1e9 + beta
		id := anchorIDByV[v]
		if i > 0 && rng.float() < genAnomaly { // 首锚点保持干净，保证零读数锚存在
			mag := 3_000_000 + rng.float()*3_000_000
			if rng.float() < 0.5 {
				mag = -mag
			}
			a += mag
			truth.BadAnchors++
		}
		idc := id
		pts = append(pts, audioPoint{ns: a, sync: &idc})
	}
	// 普通 10ms 网格点：读数从 0 起（音频首点归零），落在任一锚点保护带内则跳过。
	upperNS := alpha*float64(cfg.Seconds)*1e9 + beta
	for a := 0.0; a <= upperNS; a += audioStepS * 1e9 {
		near := false
		for _, v := range anchorV {
			anchorA := alpha*v*1e9 + beta
			if math.Abs(a-anchorA) < guardMS*1e9 {
				near = true
				break
			}
		}
		if !near {
			pts = append(pts, audioPoint{ns: a})
		}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].ns < pts[j].ns })

	dir := filepath.Dir(cfg.OutPath)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	tmp, err := os.CreateTemp(dir, ".avsync-gen-*.tmp")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	abort := func() { tmp.Close(); os.Remove(tmpName) }

	bw := bufio.NewWriterSize(tmp, 1<<20)
	enc := json.NewEncoder(bw)
	writeRec := func(track string, seq, pts int64, num, den int64, bits int, sync *string) error {
		return enc.Encode(RawRecord{
			Track: track, Epoch: 0, Seq: seq, PTS: pts,
			Num: num, Den: den, Bits: bits, Sync: sync,
		})
	}

	// ---- 视频轨：25/50fps 交替，µs 计数器，32 位回绕 ----
	var seqV, prevRawV, wrapsV int64
	for t := 0.0; t < float64(cfg.Seconds); {
		tick := roundEven(t * videoTB)
		raw := tick & (period32 - 1)
		if seqV > 0 && raw < prevRawV {
			wrapsV++
		}
		var sync *string
		if isAnchorTime(t) {
			if id, ok := anchorIDByV[math.Round(t/anchorEvery)*anchorEvery]; ok {
				s := id
				sync = &s
			}
		}
		if err := writeRec("video", seqV, raw, 1, videoTB, 32, sync); err != nil {
			abort()
			return nil, err
		}
		prevRawV = raw
		seqV++
		fps := 25.0
		if int(math.Floor(t/segSeconds))%2 == 1 {
			fps = 50.0
		}
		t = math.Round((t+1.0/fps)*1e6) / 1e6 // 微秒网格对齐，杜绝浮点漂移
	}
	truth.VideoCount = int(seqV)
	truth.VideoWraps = int(wrapsV)

	// ---- 音频轨输出 ----
	var seqA, prevRawA, wrapsA int64
	for _, p := range pts {
		tick := roundEven(p.ns / 1e9 * audioTB)
		raw := tick & (period32 - 1)
		if seqA > 0 && raw < prevRawA {
			wrapsA++
		}
		if err := writeRec("audio", seqA, raw, 1, audioTB, 32, p.sync); err != nil {
			abort()
			return nil, err
		}
		prevRawA = raw
		seqA++
	}
	truth.AudioCount = int(seqA)
	truth.AudioWraps = int(wrapsA)

	if err := bw.Flush(); err != nil {
		abort()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, err
	}
	if err := os.Rename(tmpName, cfg.OutPath); err != nil {
		os.Remove(tmpName)
		return nil, err
	}
	return truth, nil
}

func isAnchorTime(t float64) bool {
	k := math.Round(t / anchorEvery)
	return math.Abs(t-k*anchorEvery) < 1e-9
}

func anchorID(t float64) string {
	return "sync-" + itoaPad(int64(math.Round(t/anchorEvery)))
}

// itoaPad 生成至少 5 位宽的零填充十进制编号。
func itoaPad(v int64) string {
	const digits = "0123456789"
	var buf [20]byte
	i := len(buf)
	for {
		i--
		buf[i] = digits[v%10]
		v /= 10
		if v == 0 {
			break
		}
	}
	for len(buf)-i < 5 {
		i--
		buf[i] = '0'
	}
	return string(buf[i:])
}

type genArgError string

func (e genArgError) Error() string   { return string(e) }
func errInvalidGenArg(s string) error { return genArgError(s) }
