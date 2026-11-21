package clock

import "testing"

func TestUnwrapMonotonic(t *testing.T) {
	u := NewUnwrapper(0)
	seq := []uint32{1000, 1033, 1066, 1099}
	var prev uint64
	for i, ts := range seq {
		ext, back := u.Extend(ts)
		if back {
			t.Fatalf("frame %d 不应判定为倒退", i)
		}
		if i > 0 && ext <= prev {
			t.Fatalf("扩展 tick 非单调: %d -> %d", prev, ext)
		}
		prev = ext
	}
}

func TestUnwrapWrapAround(t *testing.T) {
	const mod = uint64(1 << 32)
	u := NewUnwrapper(mod)
	first, _ := u.Extend(uint32(mod - 10))
	second, back := u.Extend(5)
	if back {
		t.Fatal("前向回绕不应标记倒退")
	}
	if want := uint64(mod + 5); second != want {
		t.Fatalf("回绕扩展错误: got %d want %d", second, want)
	}
	if second != first+15 {
		t.Fatalf("回绕后相对跳变应为 15: %d -> %d", first, second)
	}
	// 自定义小回绕模，验证参数化回绕点。
	u2 := NewUnwrapper(1000)
	a, _ := u2.Extend(990)
	b, _ := u2.Extend(10)
	if b != a+20 {
		t.Fatalf("自定义模回绕错误: %d -> %d", a, b)
	}
}

func TestUnwrapBackward(t *testing.T) {
	u := NewUnwrapper(0)
	u.Extend(100)
	// raw 向前跳过半程等价于媒体时间倒退（RFC3550 半程门限）。
	_, back := u.Extend(1<<31 + 1000)
	if !back {
		t.Fatal("半程以上的时间戳倒退未被识别")
	}
	ext, back := u.Extend(1<<31 + 1033)
	if back || ext <= 0 {
		t.Fatalf("倒退后恢复前向失败: ext=%d back=%v", ext, back)
	}
}
