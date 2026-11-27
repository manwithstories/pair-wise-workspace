package ingest

import (
	"fmt"

	"bcastqc/internal/model"
)

// BandwidthPlan 回放一段持续变化的带宽计划（单位 bit/s）。
// 计划按固定时间片轮转，每个时间片使用一个带宽档位；
// 低带宽档位会让传输队列积压，超出链路缓存的包表现为突发丢失。
type BandwidthPlan struct {
	// Levels 为按时间片排列的带宽档位（bit/s）。
	Levels []int64
	// SliceMs 为每个档位持续的毫秒数。
	SliceMs int64
	// PacketBits 为单个包在线路上的等效比特数（含载荷与协议头估算）。
	PacketBits int64
}

// NewBandwidthPlan 校验并构造带宽计划。
func NewBandwidthPlan(levels []int64, sliceMs, packetBits int64) (*BandwidthPlan, error) {
	if len(levels) == 0 {
		return nil, fmt.Errorf("%w: 带宽计划至少需要一个档位", model.ErrInvalidInput)
	}
	if sliceMs <= 0 {
		return nil, fmt.Errorf("%w: 时间片长度必须为正", model.ErrInvalidInput)
	}
	if packetBits <= 0 {
		return nil, fmt.Errorf("%w: 包比特数必须为正", model.ErrInvalidInput)
	}
	for i, lv := range levels {
		if lv < 0 {
			return nil, fmt.Errorf("%w: 第 %d 档带宽为负 %d", model.ErrInvalidInput, i, lv)
		}
	}
	return &BandwidthPlan{Levels: levels, SliceMs: sliceMs, PacketBits: packetBits}, nil
}

// LevelAt 返回给定毫秒时刻生效的带宽档位。
func (b *BandwidthPlan) LevelAt(nowMs int64) int64 {
	idx := int(nowMs/b.SliceMs) % len(b.Levels)
	return b.Levels[idx]
}

// CapacityAt 返回某时间片内可完成传输的包数（向下取整）。
// 采用整数运算避免浮点边界误差：level * sliceMs / 1000 / packetBits。
func (b *BandwidthPlan) CapacityAt(nowMs int64) int64 {
	level := b.LevelAt(nowMs)
	if level <= 0 {
		return 0
	}
	bits, err := model.Mul64Checked(level, b.SliceMs)
	if err != nil {
		// 溢出视为容量受限，交由窗口层表现为丢失而非崩溃。
		return 0
	}
	return bits / 1000 / b.PacketBits
}

// SimulateDelivery 按带宽计划回放有序包序列，返回实际到达顺序。
// 每个时间片注入 injectPerSlice 个新包（模拟恒定码率采集），
// 再按当前档位容量放行队首若干包；低带宽档放行能力低于注入速率，
// 队列持续积压，长度超过 queueCap 时最旧排队包被挤出（突发丢失）。
// 被丢弃的包不会出现在结果中，交由窗口组装器判缺，且不影响后续采样位置。
func (b *BandwidthPlan) SimulateDelivery(packets []Packet, queueCap, injectPerSlice int64) []Packet {
	if queueCap < 1 {
		queueCap = 1
	}
	if injectPerSlice < 1 {
		injectPerSlice = 1
	}
	arrived := make([]Packet, 0, len(packets))
	queue := make([]Packet, 0, queueCap)
	step := int64(0)
	for cursor := 0; cursor < len(packets); step++ {
		// 1) 当前片注入新包，溢出缓存的最旧包直接丢失。
		for k := int64(0); k < injectPerSlice && cursor < len(packets); k++ {
			if int64(len(queue)) >= queueCap {
				queue = queue[1:]
			}
			queue = append(queue, packets[cursor])
			cursor++
		}
		// 2) 按当前带宽容量放行队首包。
		capacity := b.CapacityAt(step * b.SliceMs)
		for capacity > 0 && len(queue) > 0 {
			arrived = append(arrived, queue[0])
			queue = queue[1:]
			capacity--
		}
	}
	// 收尾：链路恢复后排空剩余队列。
	arrived = append(arrived, queue...)
	return arrived
}
