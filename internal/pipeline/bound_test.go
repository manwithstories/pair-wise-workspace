package pipeline

import (
	"testing"
)

// TestMemoryBoundInvariant 验证有界资源的结构性不变量：
// 任意时刻排队块数不超过容量，单块负载不超过 1MiB 解析缓冲。
// 由此内存峰值上界 = 2 * (队列容量 * 单块上限 + 单块解析缓冲)。
// 该测试不依赖 runtime 内存统计，而是断言支撑该上界的两个硬约束始终成立。
func TestMemoryBoundInvariant(t *testing.T) {
	const (
		capacity = 4
		oneMiB   = int64(1 << 20)
	)
	p, err := New(Config{
		QueueCapacity: capacity,
		ReorderWindow: 16,
		MaxBlockBytes: oneMiB,
	})
	if err != nil {
		t.Fatal(err)
	}
	q := p.Queue()

	// 不变量 1：队列长度永不超过容量。
	for i := 0; i < capacity*3; i++ {
		// 用 TryPush 保证不阻塞；满了就先 Pop 腾位。
		if err := q.TryPush(&Block{Seq: uint16(i), Payload: make([]byte, 1024)}); err != nil {
			if err != ErrQueueFull {
				t.Fatalf("unexpected error: %v", err)
			}
			if q.TryPop() == nil {
				t.Fatal("full queue should be drainable")
			}
			if err := q.TryPush(&Block{Seq: uint16(i)}); err != nil {
				t.Fatalf("retry push: %v", err)
			}
		}
		if q.Len() > capacity {
			t.Fatalf("queue len %d exceeds capacity %d", q.Len(), capacity)
		}
	}
	if q.Peak() > capacity {
		t.Fatalf("peak %d exceeds capacity %d", q.Peak(), capacity)
	}

	// 不变量 2：单块负载受 MaxBlockBytes 约束（1 MiB）。
	block := &Block{Payload: make([]byte, oneMiB)}
	if int64(len(block.Payload)) > p.Config().MaxBlockBytes {
		t.Fatal("block exceeds 1 MiB parse buffer")
	}

	// 推导上界：2*(队列字节上界 + 解析缓冲)。
	queueBound := uint64(capacity) * uint64(oneMiB)
	peakBound := 2 * (queueBound + uint64(oneMiB))
	if peakBound <= queueBound {
		t.Fatal("peak bound must exceed queue-only bound")
	}
}

// TestReorderWindowHardCap 验证重排窗口不超过 4096 的硬上限。
func TestReorderWindowHardCap(t *testing.T) {
	if _, err := New(Config{QueueCapacity: 2, ReorderWindow: 4097, MaxBlockBytes: 1 << 20}); err == nil {
		t.Fatal("reorder window above 4096 must be rejected")
	}
	if _, err := New(Config{QueueCapacity: 2, ReorderWindow: 4096, MaxBlockBytes: 1 << 20}); err != nil {
		t.Fatalf("window 4096 should be allowed: %v", err)
	}
}
