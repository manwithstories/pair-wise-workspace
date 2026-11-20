package align

import "testing"

// 构造三层表示层：关键帧每 10 秒，层间带小幅确定性时间偏差。
func buildReps(jitter []int64) []Representation {
	reps := make([]Representation, 3)
	for r := range reps {
		reps[r].ID = string(rune('A' + r))
		for f := 0; f < 600; f++ {
			isKF := f%300 == 0 // 10 秒一个关键帧（30fps）
			us := int64(f) * 33366
			if isKF {
				us += jitter[r]
			}
			reps[r].Frames = append(reps[r].Frames, Frame{
				AUIndex:   f,
				Micros:    us,
				Keyframe:  isKF,
				ByteStart: int64(f * 100),
				ByteEnd:   int64((f + 1) * 100),
			})
		}
	}
	return reps
}

func TestAlignWithinThreshold(t *testing.T) {
	// 层间最大抖动 20ms，落在 40ms 阈值内。
	reps := buildReps([]int64{0, 10_000, 20_000})
	res, err := Align(reps, Options{
		MaxDriftMicros: 40_000,
		TargetSegmentUS: 10_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failures) != 0 {
		t.Fatalf("阈值内不应有失败: %+v", res.Failures)
	}
	if len(res.Cuts) < 2 {
		t.Fatalf("切点数=%d, 期望至少 2", len(res.Cuts))
	}
	if res.MaxDrift > 40_000 {
		t.Fatalf("最大偏差=%d 超过阈值", res.MaxDrift)
	}
	// 每个切点必须三层齐备且都指向关键帧。
	for _, cut := range res.Cuts {
		if len(cut.Points) != 3 {
			t.Fatalf("切点 %d 未覆盖全部表示层", cut.Index)
		}
	}
}

func TestAlignExplicitFailureBeyondThreshold(t *testing.T) {
	// 层 C 抖动 50ms，超过 40ms 阈值，必须显式失败而非挪动。
	reps := buildReps([]int64{0, 5_000, 50_000})
	res, err := Align(reps, Options{
		MaxDriftMicros:  40_000,
		TargetSegmentUS: 10_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failures) == 0 {
		t.Fatalf("超阈偏差必须产生显式失败")
	}
	// 失败切点不得出现在成功 Cuts 中（不静默挪动）。
	for _, f := range res.Failures {
		if f.DriftMicros <= 40_000 {
			t.Fatalf("失败记录漂移=%d 未超阈", f.DriftMicros)
		}
		for _, c := range res.Cuts {
			if c.AnchorMicros == f.NearMicros {
				t.Fatalf("失败切点被静默纳入 Cuts")
			}
		}
	}
}

func TestAlignRejectsLayerWithoutKeyframes(t *testing.T) {
	reps := []Representation{
		{ID: "A", Frames: []Frame{{AUIndex: 0, Micros: 0, Keyframe: true}}},
		{ID: "B", Frames: []Frame{{AUIndex: 0, Micros: 0, Keyframe: false}}},
	}
	if _, err := Align(reps, Options{}); err == nil {
		t.Fatalf("无关键帧层必须报错")
	}
}

func TestAlignEmptyInput(t *testing.T) {
	if _, err := Align(nil, Options{}); err == nil {
		t.Fatalf("空输入必须报错")
	}
}

func TestNearestKeyframeBinarySearch(t *testing.T) {
	kfs := []Frame{
		{Micros: 0}, {Micros: 100_000}, {Micros: 200_000},
	}
	if f, d, _ := nearestKeyframe(kfs, 90_000); f.Micros != 100_000 || d != 10_000 {
		t.Fatalf("最近帧选择错误: micros=%d drift=%d", f.Micros, d)
	}
	if f, d, _ := nearestKeyframe(kfs, 40_000); f.Micros != 0 || d != -40_000 {
		t.Fatalf("前向最近帧错误: micros=%d drift=%d", f.Micros, d)
	}
}
