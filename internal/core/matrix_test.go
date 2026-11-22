package core

import (
	"testing"

	"mediacheck/internal/model"
)

func TestMatrixPassthrough(t *testing.T) {
	m, err := BuildMatrix(model.LayoutStereo, model.LayoutStereo, true)
	if err != nil {
		t.Fatal(err)
	}
	in := []int32{1000, -2000}
	out, err := m.Apply(in, 1)
	if err != nil || out[0] != 1000 || out[1] != -2000 {
		t.Fatalf("passthrough = %v, %v", out, err)
	}
}

func TestMatrix51DownmixPreservesAllChannels(t *testing.T) {
	m, _ := BuildMatrix(model.Layout51, model.LayoutStereo, true)
	// 每个源声道给一个非零值，确认都参与混音（无静默丢声道）。
	in := []int32{1000, 1000, 2000, 100, 500, 500}
	out, err := m.Apply(in, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("downmix channels = %d", len(out))
	}
	if out[0] == 0 || out[1] == 0 {
		t.Fatal("downmix produced silence, channel contribution lost")
	}
	// 左右应近似对称（输入对称时）。
	diff := out[0] - out[1]
	if diff < -2 || diff > 2 {
		t.Fatalf("symmetric downmix not balanced: L=%d R=%d", out[0], out[1])
	}
}

func TestMatrixUpmixFillsSilence(t *testing.T) {
	m, _ := BuildMatrix(model.LayoutStereo, model.Layout71, true)
	out, err := m.Apply([]int32{100, -100}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 8 || out[0] != 100 || out[1] != -100 {
		t.Fatalf("upmix front = %v", out)
	}
	for i := 2; i < 8; i++ {
		if out[i] != 0 {
			t.Fatalf("upmixed channel %d must be silent, got %d", i, out[i])
		}
	}
}

func TestMatrixMissingChannelsPolicy(t *testing.T) {
	m, _ := BuildMatrix(model.Layout51, model.LayoutStereo, false)
	// 只给 2 个声道，fill 关闭 → 拒绝。
	if _, err := m.Apply([]int32{1, 2}, 7); err == nil {
		t.Fatal("missing channels must error when fill disabled")
	} else if me, ok := model.AsMediaError(err); !ok || me.TrackID != 7 {
		t.Fatalf("error must report track id, got %v", err)
	}
	// fill 开启 → 缺的补静音，可正常处理。
	m2, _ := BuildMatrix(model.Layout51, model.LayoutStereo, true)
	if _, err := m2.Apply([]int32{1, 2}, 7); err != nil {
		t.Fatalf("fill-silence should succeed: %v", err)
	}
}

func TestMatrix71To51(t *testing.T) {
	m, _ := BuildMatrix(model.Layout71, model.Layout51, true)
	in := []int32{10, 20, 30, 40, 50, 60, 70, 80}
	out, err := m.Apply(in, 1)
	if err != nil || len(out) != 6 {
		t.Fatalf("7.1->5.1 = %v, %v", out, err)
	}
	// 后环绕 Lrs/Rrs 应并入 Ls/Rs（下标 4/5）。
	if out[4] == 50 || out[5] == 60 {
		t.Fatal("rear surrounds were not folded into side surrounds")
	}
}

func TestMatrixUnknownLayoutRejected(t *testing.T) {
	if _, err := BuildMatrix("atmos", model.LayoutStereo, true); err == nil {
		t.Fatal("unknown layout must be rejected")
	}
}
