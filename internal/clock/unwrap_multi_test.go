package clock

import "testing"

// TestUnwrapMultipleWraps 验证连续多次回绕后扩展值仍保持单调且差值正确。
func TestUnwrapMultipleWraps(t *testing.T) {
	const mod = uint64(1 << 32)
	u := NewUnwrapper(mod)
	var prev uint64
	first := true
	// 从接近回绕点开始，每步前进 60 tick，连续跨越回绕边界。
	ts := uint32(mod - 100)
	for i := 0; i < 6; i++ {
		ext, back := u.Extend(ts)
		if back {
			t.Fatalf("第 %d 帧不应倒退", i)
		}
		if !first && ext != prev+60 {
			t.Fatalf("多次回绕后跳变错误: %d -> %d", prev, ext)
		}
		prev = ext
		first = false
		ts += 60
	}
}
