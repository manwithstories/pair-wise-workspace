package probe

import (
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"bcast/internal/fault"
	"bcast/internal/model"
)

// DefaultTracks 构造三路 VFR 画面与 Opus/AAC 双音轨的标准编排。
func DefaultTracks() []model.TrackPlan {
	return []model.TrackPlan{
		{ID: 0, Kind: model.KindVideo, Timebase: 90_000, PeriodUS: 33_333, JitterUS: 4_000, DriftPPM: 120},
		{ID: 1, Kind: model.KindVideo, Timebase: 25, PeriodUS: 40_000, JitterUS: 2_500, DriftPPM: -80},
		{ID: 2, Kind: model.KindVideo, Timebase: 30_000, PeriodUS: 33_367, JitterUS: 3_000, DriftPPM: 40},
		{ID: 3, Kind: model.KindOpus, Timebase: 48_000, PeriodUS: 20_000, JitterUS: 800, DriftPPM: 60},
		{ID: 4, Kind: model.KindAAC, Timebase: 90_000, PeriodUS: 21_333, JitterUS: 0, DriftPPM: -45},
	}
}

// DefaultWrapModulus 让每条轨道在 30 分钟素材内经历多次回绕。
func DefaultWrapModulus(tracks []model.TrackPlan) map[uint8]uint64 {
	m := make(map[uint8]uint64, len(tracks))
	for _, t := range tracks {
		if t.Kind == model.KindVideo {
			m[t.ID] = 1 << 17 // 131072
		} else {
			m[t.ID] = 1 << 14 // 16384
		}
	}
	return m
}

// DefaultFaultPlan 覆盖四类必备故障（截断/校验/回绕/漂移叠加VFR）。
func DefaultFaultPlan(enableAll bool) model.FaultPlan {
	if !enableAll {
		return model.FaultPlan{}
	}
	return model.FaultPlan{
		EnableDrops: true, EnableDupes: true, EnableCorrupt: true, EnableTrunc: true,
		DropCount: 12, DupeCount: 10, CorruptCount: 3, TruncCount: 2,
	}
}

// lcg 是显式确定性随机源，不依赖全局 rand 状态，保证跨进程可复现。
type lcg struct{ state uint64 }

func newLCG(seed int64, salt uint64) *lcg {
	return &lcg{state: uint64(seed)*6364136223846793005 + salt*1442695040888963407 + 1}
}
func (r *lcg) next() uint64 {
	r.state = r.state*6364136223846793005 + 1442695040888963407
	return r.state
}
func (r *lcg) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

// pickTargets 在 [0,size) 内选取 count 个互不相同且避开首末包的下标，有序返回。
func pickTargets(rng *lcg, size, count int) []int {
	if count*2 > size {
		count = size / 3
	}
	seen := make(map[int]bool, count)
	out := make([]int, 0, count)
	for len(out) < count {
		idx := 1 + rng.intn(size-2)
		if !seen[idx] {
			seen[idx] = true
			out = append(out, idx)
		}
	}
	// 插入排序，目标列表保持确定顺序。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Generate 按 Plan 生成确定性素材到 workDir/master.bin，返回写入字节数。
func Generate(workDir string, p model.Plan) (int64, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return 0, err
	}
	path := filepath.Join(workDir, "master.bin")
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err := WriteHeader(f, p); err != nil {
		return 0, err
	}

	totalUS := p.DurationMS * 1000
	var totalPackets uint64
	for _, t := range p.Tracks {
		rng := newLCG(p.Seed, uint64(t.ID)*7919+13)
		// 预估包数以挑选故障下标。
		est := int(totalUS/t.PeriodUS) + 2
		dropT := map[int]bool{}
		dupeT := map[int]bool{}
		corT := map[int]bool{}
		truncT := map[int]bool{}
		if p.Faults.EnableDrops {
			for _, i := range pickTargets(newLCG(p.Seed, 101+uint64(t.ID)), est, p.Faults.DropCount) {
				dropT[i] = true
			}
		}
		if p.Faults.EnableDupes {
			for _, i := range pickTargets(newLCG(p.Seed, 202+uint64(t.ID)), est, p.Faults.DupeCount) {
				dupeT[i] = true
			}
		}
		if p.Faults.EnableCorrupt {
			for _, i := range pickTargets(newLCG(p.Seed, 303+uint64(t.ID)), est, p.Faults.CorruptCount) {
				corT[i] = true
			}
		}
		if p.Faults.EnableTrunc {
			for _, i := range pickTargets(newLCG(p.Seed, 404+uint64(t.ID)), est, p.Faults.TruncCount) {
				truncT[i] = true
			}
		}

		var clockUS int64
		idx := 0
		for clockUS < totalUS {
			// 漂移：线性 ppm 累积；VFR：对称抖动，均为确定值。
			driftUS := clockUS * t.DriftPPM / 1_000_000
			jitter := int64(0)
			if t.JitterUS > 0 {
				jitter = int64(rng.next()%uint64(2*t.JitterUS+1)) - t.JitterUS
			}
			durUS := t.PeriodUS + jitter
			if durUS < t.PeriodUS/2 {
				durUS = t.PeriodUS / 2
			}
			nowUS := clockUS + driftUS
			pts, err := scaleUp(nowUS, t.Timebase)
			if err != nil {
				return 0, err
			}
			dur, err := scaleUp(durUS, t.Timebase)
			if err != nil {
				return 0, err
			}
			payload := buildPayload(t, uint64(idx), uint64(pts))
			seq := uint64(idx)
			isDrop := dropT[idx]
			isDupe := dupeT[idx]
			isCorrupt := corT[idx]
			isTrunc := truncT[idx]
			if !isDrop {
				if err := writeRecord(f, t, seq, pts, dur, payload, p.WrapModulus[t.ID], isCorrupt, isTrunc); err != nil {
					return 0, err
				}
				totalPackets++
				if isDupe { // 重复包：同序号同 PTS 再写一次
					if err := writeRecord(f, t, seq, pts, dur, payload, p.WrapModulus[t.ID], false, false); err != nil {
						return 0, err
					}
					totalPackets++
				}
			}
			clockUS += durUS
			idx++
		}
	}
	// 回填包总数。
	if _, err := f.Seek(HeaderSize(len(p.Tracks))-8, io.SeekStart); err != nil {
		return 0, err
	}
	var tb [8]byte
	u64(tb[:], totalPackets)
	if _, err := f.Write(tb[:]); err != nil {
		return 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func scaleUp(us int64, timebase uint64) (uint64, error) {
	// 主时基微秒 -> 轨道滴答，与 clock.ConvertU 一致的溢出语义。
	if us < 0 || us > (1<<63-1)/int64(timebase) {
		return 0, fault.New(fault.ErrBoundary, fault.KindWrap, 0, 0, 0, 0, "fixture pts overflow")
	}
	return uint64(us) * timebase / 1_000_000, nil
}

func buildPayload(t model.TrackPlan, seq, pts uint64) []byte {
	n := VideoPayloadLen
	if t.Kind != model.KindVideo {
		n = AudioPayloadLen
	}
	b := make([]byte, n)
	copy(b, FrameMagic)
	binary.BigEndian.PutUint64(b[4:12], seq)
	binary.BigEndian.PutUint64(b[12:20], pts)
	for i := 20; i < n; i++ {
		b[i] = byte((seq*31 + uint64(i)*17 + pts) & 0xff)
	}
	// 保证包体不含同步字序列，使读取器能用同步字可靠定界。
	for i := 0; i+4 <= n; i++ {
		if binary.BigEndian.Uint32(b[i:i+4]) == SyncWord {
			b[i] ^= 0xFF
		}
	}
	return b
}

func writeRecord(w io.Writer, t model.TrackPlan, seq, pts, dur uint64,
	payload []byte, modulus uint64, corrupt, truncated bool) error {
	if len(payload) > MaxPacketLen {
		return fault.InvalidInputf("packet %d exceeds %d bytes", len(payload), MaxPacketLen)
	}
	crc := crc32.ChecksumIEEE(payload)
	hdr := make([]byte, RecHdrLen)
	u32(hdr[0:4], SyncWord)
	hdr[4] = t.ID
	hdr[5] = uint8(t.Kind)
	u64(hdr[6:14], seq)
	u64(hdr[14:22], pts%modulus) // 原始 PTS 落在回绕域内
	u32(hdr[22:26], uint32(dur))
	u32(hdr[26:30], uint32(len(payload)))
	u32(hdr[30:34], crc)
	if corrupt {
		hdr[33] ^= 0xFF // 破坏 CRC 字节 -> 校验失败
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	body := payload
	if truncated {
		body = payload[:len(payload)/2] // 截断：声明长度与实写字节不符
	}
	_, err := w.Write(body)
	return err
}
