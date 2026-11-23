package rtp

import (
	"fmt"

	pionrtp "github.com/pion/rtp"
)

// RetransmitCache 是容量受限的 RTP 重传缓存（FIFO 淘汰）。
type RetransmitCache struct {
	cap   int
	order []uint16
	pkts  map[uint16]*pionrtp.Packet
}

// NewRetransmitCache 创建容量为 capacity 的重传缓存。
func NewRetransmitCache(capacity int) *RetransmitCache {
	if capacity < 1 {
		capacity = 1
	}
	return &RetransmitCache{cap: capacity, pkts: make(map[uint16]*pionrtp.Packet)}
}

// Add 缓存一个已发送包；重复添加幂等。
func (c *RetransmitCache) Add(pkt *pionrtp.Packet) {
	if _, ok := c.pkts[pkt.SequenceNumber]; ok {
		return
	}
	if len(c.order) >= c.cap {
		delete(c.pkts, c.order[0])
		c.order = c.order[1:]
	}
	c.order = append(c.order, pkt.SequenceNumber)
	c.pkts[pkt.SequenceNumber] = pkt
}

// Get 按序号取回缓存包。
func (c *RetransmitCache) Get(seq uint16) (*pionrtp.Packet, bool) {
	pkt, ok := c.pkts[seq]
	return pkt, ok
}

// Len 返回当前缓存包数。
func (c *RetransmitCache) Len() int { return len(c.pkts) }

// Gap 记录一段无法恢复的空洞及原因；恢复层绝不伪造帧。
type Gap struct {
	From   uint16
	To     uint16
	Reason string
}

func (g Gap) String() string {
	return fmt.Sprintf("seq %d..%d: %s", g.From, g.To, g.Reason)
}

// Recovery 用重传缓存恢复突发丢包。
type Recovery struct {
	Cache     *RetransmitCache
	Recovered int
	Gaps      []Gap
}

// NewRecovery 创建恢复器，重传缓存容量为 capacity。
func NewRecovery(capacity int) *Recovery {
	return &Recovery{Cache: NewRetransmitCache(capacity)}
}

// Recover 尝试恢复缺失序号（升序）。命中重传缓存的返回；
// 无法恢复的连续段合并为带原因的空洞记录。
func (r *Recovery) Recover(missing []uint16) []*pionrtp.Packet {
	var out []*pionrtp.Packet
	gapOpen := false
	for _, seq := range missing {
		if pkt, ok := r.Cache.Get(seq); ok {
			out = append(out, pkt)
			r.Recovered++
			gapOpen = false
			continue
		}
		if !gapOpen {
			r.Gaps = append(r.Gaps, Gap{From: seq, Reason: "retransmit cache miss"})
			gapOpen = true
		}
		r.Gaps[len(r.Gaps)-1].To = seq
	}
	return out
}

// Lost 返回空洞覆盖的包数。
func (r *Recovery) Lost() int {
	n := 0
	for _, g := range r.Gaps {
		n += int(g.To-g.From) + 1
	}
	return n
}
