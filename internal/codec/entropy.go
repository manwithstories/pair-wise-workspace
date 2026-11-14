package codec

import "math/bits"

// uexpBits 返回非负整数 v 的指数哥伦布码字长度（比特）。
func uexpBits(v int) int {
	n := uint32(v + 1)
	return 2*bits.Len32(n) - 1
}

// sexpBits 返回有符号整数 v 的指数哥伦布码字长度（比特）。
// 映射：0->0, 1->1, -1->2, 2->3, -2->4 ...
func sexpBits(v int) int {
	u := 2*v - 1
	if v <= 0 {
		u = -2 * v
	}
	return uexpBits(u)
}

// rlePair 表示一个 (游程, 非零系数) 对。
type rlePair struct {
	run, level int
}

// blockRLC 对量化系数块做 zigzag 游程编码，返回 (run,level) 序列。
// q 为自然行优先存储；DC 单独处理，不在序列中。
func blockRLC(q *[64]int) []rlePair {
	pairs := make([]rlePair, 0, 8)
	run := 0
	for i := 1; i < 64; i++ {
		c := q[zigzag[i]]
		if c == 0 {
			run++
			continue
		}
		pairs = append(pairs, rlePair{run: run, level: c})
		run = 0
	}
	return pairs
}

// blockBits 计算一个量化块的熵编码比特数：
// DC 用有符号指数哥伦布，AC 用 (run,level) 对，块尾补 EOB。
func blockBits(q *[64]int) int64 {
	n := int64(sexpBits(q[0]))
	for _, p := range blockRLC(q) {
		n += int64(uexpBits(p.run) + sexpBits(p.level))
	}
	return n + int64(uexpBits(0)) // EOB
}
