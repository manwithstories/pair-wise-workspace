// Package packet 重组乱序数据包并以分组 XOR 校验恢复丢包；
// 恢复结果必须通过 CRC 校验，损坏时不得伪报恢复。
package packet

import (
	"hash/crc32"
	"sort"

	"review/internal/fault"
)

// GroupSize 每组数据包数，另加 1 个校验包。
const GroupSize = 4

// Packet 一个数据或校验包。
type Packet struct {
	Group   int
	Seq     int  // 组内序号 0..GroupSize-1；校验包 Seq == GroupSize
	Parity  bool // 是否为 XOR 校验包
	Len     int  // 数据包：负载长度；校验包：组内数据长度 XOR
	Payload []byte
	CRC     uint32 // 数据包：负载 CRC32；校验包：组内数据 CRC 的 XOR
}

// MakeGroup 把一组负载打包为 GroupSize 个数据包加 1 个校验包。
func MakeGroup(group int, payloads [][]byte) []Packet {
	maxLen := 0
	for _, p := range payloads {
		if len(p) > maxLen {
			maxLen = len(p)
		}
	}
	parity := make([]byte, maxLen)
	var crcXOR, lenXOR uint32
	pkts := make([]Packet, 0, GroupSize+1)
	for i, p := range payloads {
		for j, b := range p {
			parity[j] ^= b
		}
		crcXOR ^= crc32.ChecksumIEEE(p)
		lenXOR ^= uint32(len(p))
		pkts = append(pkts, Packet{Group: group, Seq: i, Len: len(p),
			Payload: p, CRC: crc32.ChecksumIEEE(p)})
	}
	pkts = append(pkts, Packet{Group: group, Seq: GroupSize, Parity: true,
		Len: int(lenXOR), Payload: parity, CRC: crcXOR})
	return pkts
}

// Reorder 按 (Group, Seq) 排序乱序包。
func Reorder(pkts []Packet) []Packet {
	out := make([]Packet, len(pkts))
	copy(out, pkts)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Seq < out[j].Seq
	})
	return out
}

// Result 恢复结果。
type Result struct {
	Payloads  [][]byte // 按组序展开的数据负载
	Lost      int      // 检测到的丢失数据包数
	Recovered int      // 成功恢复数
}

// Recover 校验并恢复各组；单包丢失用校验包重建并验证 CRC，
// 多包丢失或校验损坏报不可恢复故障，绝不伪报恢复。
func Recover(pkts []Packet) (Result, []*fault.Error) {
	var res Result
	var errs []*fault.Error
	groups := map[int][]Packet{}
	maxGroup := -1
	for _, p := range Reorder(pkts) {
		groups[p.Group] = append(groups[p.Group], p)
		if p.Group > maxGroup {
			maxGroup = p.Group
		}
	}
	for g := 0; g <= maxGroup; g++ {
		pk, ok := groups[g]
		if !ok {
			errs = append(errs, fault.Newf(fault.KindPacketLoss, "group", int64(g), false,
				"group %d entirely missing", g))
			res.Lost += GroupSize
			continue
		}
		data := make([]*Packet, GroupSize)
		var parity *Packet
		present := 0
		for i := range pk {
			p := pk[i]
			if p.Parity {
				parity = &pk[i]
				continue
			}
			if p.Seq >= 0 && p.Seq < GroupSize {
				data[p.Seq] = &pk[i]
				present++
			}
		}
		missing := GroupSize - present
		res.Lost += missing
		switch {
		case missing == 0:
			for _, p := range data {
				if crc32.ChecksumIEEE(p.Payload) != p.CRC {
					errs = append(errs, fault.Newf(fault.KindCRC, "group", int64(g), true,
						"data packet crc mismatch seq=%d", p.Seq))
					continue
				}
				res.Payloads = append(res.Payloads, p.Payload)
			}
		case missing == 1 && parity != nil:
			var mi int
			for i, p := range data {
				if p == nil {
					mi = i
				}
			}
			rec, crc, ln := reconstruct(data, parity)
			if crc32.ChecksumIEEE(rec) != crc {
				errs = append(errs, fault.Newf(fault.KindCRC, "group", int64(g), false,
					"parity reconstruction failed crc check seq=%d", mi))
				continue
			}
			res.Recovered++
			out := make([]Packet, 0, GroupSize)
			for i, p := range data {
				if p != nil {
					out = append(out, *p)
				} else {
					out = append(out, Packet{Group: g, Seq: i, Len: ln, Payload: rec, CRC: crc})
				}
			}
			for _, p := range out {
				res.Payloads = append(res.Payloads, p.Payload)
			}
		default:
			errs = append(errs, fault.Newf(fault.KindPacketLoss, "group", int64(g), false,
				"group %d missing %d packets, parity=%t", g, missing, parity != nil))
		}
	}
	return res, errs
}

// reconstruct 用校验包与在场数据包重建缺失包，返回负载、CRC 与长度。
func reconstruct(data []*Packet, parity *Packet) ([]byte, uint32, int) {
	rec := make([]byte, len(parity.Payload))
	copy(rec, parity.Payload)
	crc := parity.CRC
	ln := uint32(parity.Len)
	for _, p := range data {
		if p == nil {
			continue
		}
		for j, b := range p.Payload {
			rec[j] ^= b
		}
		crc ^= p.CRC
		ln ^= uint32(p.Len)
	}
	if int(ln) < len(rec) {
		rec = rec[:ln]
	}
	return rec, crc, int(ln)
}
