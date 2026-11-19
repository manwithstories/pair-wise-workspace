package clock

import "testing"

func TestSeqExtenderWrapReorder(t *testing.T) {
	// 模拟 dev-a：从 65530 起，含回绕与一次相邻交换。
	raw := []uint16{65530, 65531, 65532, 65533, 65534, 65535, 1, 0, 2, 3, 4, 5}
	var ext []uint64
	var e SeqExtender
	var wraps int
	for _, s := range raw {
		x, w := e.Update(s)
		if w {
			wraps++
		}
		ext = append(ext, x)
	}
	// 期望：65535→65541，乱序的 0 应展开为 65536，1 为 65537。
	want := []uint64{65530, 65531, 65532, 65533, 65534, 65535, 65537, 65536, 65538, 65539, 65540, 65541}
	for i := range want {
		if ext[i] != want[i] {
			t.Fatalf("idx %d raw=%d got=%d want=%d", i, raw[i], ext[i], want[i])
		}
	}
	if wraps != 1 {
		t.Fatalf("want exactly 1 wrap, got %d", wraps)
	}
}

func TestTimestampUnwrapAcrossBoundary(t *testing.T) {
	var u TSUnwrapper
	near := uint32(0xFFFFFFFF) - 48
	v1, w1 := u.Update(near)
	v2, w2 := u.Update(near + 24)
	v3, w3 := u.Update(near + 72) // 越过 0xFFFFFFFF 回绕到低地址
	if w1 || w2 {
		t.Fatal("early updates must not report wrap")
	}
	if !w3 {
		t.Fatal("crossing 32bit boundary must report wrap")
	}
	if v3-v2 != 48 || v2-v1 != 24 {
		t.Fatalf("unwrapped deltas must stay monotonic: %d %d %d", v1, v2, v3)
	}
}

func TestDuplicateAndBackwardTimestamp(t *testing.T) {
	var u TSUnwrapper
	u.Update(1000)
	v, _ := u.Update(1000) // 重复时间戳：delta 0
	if v != 1000 {
		t.Fatalf("duplicate ts delta should be 0, got %d", v)
	}
	v2, _ := u.Update(999) // 倒退 1
	if v2 != 999 {
		t.Fatalf("backward ts should unwrap to 999, got %d", v2)
	}
}
