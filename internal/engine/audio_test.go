package engine

import (
	"reflect"
	"testing"

	"bcast/internal/model"
)

func TestApplyGainS16ExactAndClamps(t *testing.T) {
	gain := model.Rational{N: 3, D: 4}
	got := ApplyGainS16([]int16{0, 100, -100, 32767}, gain)
	want := []int16{0, 75, -75, 24575}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gain=%v want %v", got, want)
	}
	boost := ApplyGainS16([]int16{32767}, model.Rational{N: 2, D: 1})
	if boost[0] != 32767 {
		t.Fatalf("clamp=%d want 32767", boost[0])
	}
}
