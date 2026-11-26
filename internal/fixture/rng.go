// Package fixture 用固定种子合成 MPEG-TS 演示流及各类损坏变体，
// 不引用任何外部文件。生成结果完全确定性，相同种子产出字节一致的流。
package fixture

// rng 是内存占用极小的 xorshift64* 伪随机源，保证跨平台序列一致
// （不依赖 math/rand 的全局源版本差异）。
type rng struct {
	state uint64
}

func newRNG(seed uint64) *rng {
	if seed == 0 {
		seed = 0x9E3779B97F4A7C15
	}
	return &rng{state: seed}
}

func (r *rng) next() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545F4914F6CDD1D
}

// intn 返回 [0,n) 内的伪随机整数，n 必须为正。
func (r *rng) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

// int16n 返回 [-span,span] 内的伪随机 int16。
func (r *rng) int16Span(span int) int16 {
	v := r.intn(2*span+1) - span
	return int16(v)
}
