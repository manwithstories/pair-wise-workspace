package process

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// Entry 为待去重的片段：轨道 + 规范化媒体时间窗 + 内容摘要。
type Entry struct {
	TrackID       string
	Seg           Segment
	ContentDigest [32]byte // 窗口内样本的 SHA-256
}

// Fingerprint 按规范化媒体时间窗生成指纹：
// 窗口已统一在主时间基准（整数刻度），指纹不含轨道号，
// 因此不同轨道上同一媒体时间点、相同内容的片段指纹一致。
func Fingerprint(e Entry) [32]byte {
	h := sha256.New()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(e.Seg.Start))
	h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], uint64(e.Seg.End))
	h.Write(buf[:])
	h.Write(e.ContentDigest[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// DigestSamples 计算窗口内样本摘要。
func DigestSamples(samples []byte) [32]byte {
	return sha256.Sum256(samples)
}

// DedupResult 汇总去重结果。
type DedupResult struct {
	Kept         []Entry
	Deduplicated int
	Collisions   int // 指纹相同但内容摘要不一致（保留双方）
}

// Dedup 跨轨去重：指纹相同且内容摘要一致 → 丢弃后者；
// 指纹相同但摘要不同（哈希冲突）→ 双方保留，绝不误删内容。
func Dedup(entries []Entry) DedupResult {
	res := DedupResult{}
	seen := map[[32]byte][]int{} // 指纹 → Kept 下标列表
	for _, e := range entries {
		fp := Fingerprint(e)
		duplicate := false
		for _, idx := range seen[fp] {
			if res.Kept[idx].ContentDigest == e.ContentDigest {
				duplicate = true
				break
			}
		}
		if duplicate {
			res.Deduplicated++
			continue
		}
		if len(seen[fp]) > 0 {
			res.Collisions++
			fmt.Printf("warning: fingerprint collision between tracks %q and %q, keeping both\n",
				res.Kept[seen[fp][0]].TrackID, e.TrackID)
		}
		seen[fp] = append(seen[fp], len(res.Kept))
		res.Kept = append(res.Kept, e)
	}
	return res
}
