// Package fixture 生成自包含的样例数据：HLS/DASH 清单、fMP4 分片、
// 长度前缀 RTP 诊断记录，以及缺片、截断盒、乱序/回绕、超长分片与损坏检查点。
// 全部数据由固定种子确定性生成，不引用任何外部文件。
package fixture

import (
	"encoding/binary"
)

// Spec 描述一次夹具生成的参数。
type Spec struct {
	Seed          int64
	SegmentCount  int    // 分片总数（验收夹具为 12）
	TimeScale     uint32 // 统一媒体时间尺度
	SegmentTicks  uint32 // 每分片时长（ticks）
	SamplesPerSeg int
	RTPPerSegment int
}

// DefaultSpec 返回验收所用的 12 分片规格。
func DefaultSpec(seed int64) Spec {
	return Spec{
		Seed:          seed,
		SegmentCount:  12,
		TimeScale:     90000,
		SegmentTicks:  90000, // 每分片 1 秒
		SamplesPerSeg: 4,
		RTPPerSegment: 8,
	}
}

// rng 是确定性的 xorshift64* 随机源，保证同种子逐字节可复现。
type rng struct {
	state uint64
}

func newRNG(seed int64) *rng {
	s := uint64(seed)
	if s == 0 {
		s = 0x9E3779B97F4A7C15
	}
	return &rng{state: s}
}

func (r *rng) next() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545F4914F6CDD1D
}

func (r *rng) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

// ---- 最小 ISO-BMFF 盒构造（与 internal/mp4 测试夹具同构） ----

func box(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(payload)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

func fullBox(typ string, version uint8, flags uint32, body []byte) []byte {
	hdr := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(typ, append(hdr, body...))
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func u64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// segName 返回分片文件名（HLS 与 DASH 引用一致，便于互查）。
func segName(seq int) string {
	return "seg" + itoa(seq) + ".m4s"
}

func rtpName(seq int) string {
	return "seg" + itoa(seq) + ".rtp"
}

// itoa 避免引入 strconv 造成的额外耦合。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
