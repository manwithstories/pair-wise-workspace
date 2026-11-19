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
	t.Logf("ext=%v wraps=%d", ext, wraps)
	// 期望：65535→65541，乱序的 0 应展开为 65536，1 为 65537。
	want := []uint64{65530, 65531, 65532, 65533, 65534, 65535, 65537, 65536, 65538, 65539, 65540, 65541}
	for i := range want {
		if ext[i] != want[i] {
			t.Fatalf("idx %d raw=%d got=%d want=%d", i, raw[i], ext[i], want[i])
		}
	}
}
