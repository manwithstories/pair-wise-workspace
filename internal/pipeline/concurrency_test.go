package pipeline

import (
	"testing"

	"archivemedia/internal/model"
)

func TestSemaphoreCapacityAndAcquireRelease(t *testing.T) {
	sem, err := NewSemaphore(2)
	if err != nil || sem.Cap() != 2 {
		t.Fatalf("构造容量 2 限流器失败: %v", err)
	}
	// 占满两个槽
	sem.Acquire()
	sem.Acquire()
	// 第三个 Acquire 必须阻塞；用 goroutine + 释放验证
	acquired := make(chan struct{})
	go func() {
		sem.Acquire()
		close(acquired)
		sem.Release()
	}()
	select {
	case <-acquired:
		t.Fatal("容量已满时 Acquire 不应返回")
	default:
	}
	sem.Release() // 腾出一个槽
	<-acquired    // goroutine 获得槽
}

func TestSemaphoreRejectsBadCapacity(t *testing.T) {
	if _, err := NewSemaphore(0); model.KindOf(err) != model.KindInvalidArgument {
		t.Fatalf("容量 0 应报 invalid_argument: %v", err)
	}
	if _, err := NewSemaphore(-3); model.KindOf(err) != model.KindInvalidArgument {
		t.Fatalf("负容量应报错: %v", err)
	}
}

func TestDefaultConfigConcurrency(t *testing.T) {
	c := DefaultConfig(t.TempDir())
	if c.MaxConcurrency != 2 {
		t.Fatalf("默认并发上限应为 2, got %d", c.MaxConcurrency)
	}
	sem, err := NewSemaphore(c.MaxConcurrency)
	if err != nil {
		t.Fatal(err)
	}
	if sem.Cap() != 2 {
		t.Fatalf("信号量容量=%d", sem.Cap())
	}
}
