package fixture

import (
	"testing"

	"archivemedia/internal/adapter"
	"archivemedia/internal/model"
)

func TestGenerateAllFixtures(t *testing.T) {
	dir := t.TempDir()
	sources, err := GenerateAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 5 {
		t.Fatalf("应生成 5 个 arc1 样例, got %d", len(sources))
	}
	byKind := map[string]Source{}
	for _, s := range sources {
		byKind[s.Kind] = s
	}

	// 正常样例：双轨、含包、可完整解析
	normal, err := (&adapter.Arc1Parser{}).Parse(byKind[KindNormal].Path, adapter.DefaultLimits())
	if err != nil {
		t.Fatalf("normal 解析失败: %v", err)
	}
	if len(normal.Tracks) != 2 || len(normal.Packets) == 0 {
		t.Fatalf("normal 轨道/包异常: %s", normal)
	}

	// 截断样例：corrupt + 前缀
	trunc, err := (&adapter.Arc1Parser{}).Parse(byKind[KindTruncated].Path, adapter.DefaultLimits())
	if model.KindOf(err) != model.KindCorruptMedia || !trunc.Truncated {
		t.Fatalf("truncated 应报 corrupt 并保留前缀: %v", err)
	}
	if len(trunc.Packets) != 5 {
		t.Fatalf("应恢复 5 个前缀包, got %d", len(trunc.Packets))
	}

	// 回绕样例：1 次回绕被展开
	opts := adapter.DefaultLimits()
	opts.MaxDurationUs = 0
	wrap, err := (&adapter.Arc1Parser{}).Parse(byKind[KindWrap].Path, opts)
	if err != nil {
		t.Fatalf("wrap 解析失败: %v", err)
	}
	if len(wrap.Wraps) != 1 {
		t.Fatalf("应检测 1 次回绕, got %d", len(wrap.Wraps))
	}

	// 非法剪辑点样例：媒体本身完好
	if _, err := (&adapter.Arc1Parser{}).Parse(byKind[KindIllegalCut].Path, adapter.DefaultLimits()); err != nil {
		t.Fatalf("illegal_cut 媒体应可解析: %v", err)
	}
	var invalidPlans int
	for _, r := range byKind[KindIllegalCut].Plan.PlanRanges {
		if r.ExpectInvalid {
			invalidPlans++
		}
	}
	if invalidPlans != 2 {
		t.Fatalf("应有 2 个预期非法剪辑点, got %d", invalidPlans)
	}

	// 转场样例：双轨 + 重复声明
	trans := byKind[KindTransition]
	if len(trans.Plan.Transitions) != 1 || trans.Plan.Transitions[0].Repeat != 3 {
		t.Fatal("转场重复声明计划错误")
	}
}

func TestGenerateMKVFixture(t *testing.T) {
	dir := t.TempDir()
	s, err := GenerateMKV(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 真实容器走 astiav 适配器
	pm, err := (&adapter.AstiavParser{}).Parse(s.Path, adapter.DefaultLimits())
	if err != nil {
		t.Fatalf("mkv 解析失败: %v", err)
	}
	if len(pm.Tracks) != 2 {
		t.Fatalf("mkv 应为双轨, got %d", len(pm.Tracks))
	}
}

func TestDeterministicPayloads(t *testing.T) {
	a := gradientPayload(3, false)
	b := gradientPayload(3, false)
	if len(a) != framePayload || &a[0] == &b[0] {
		t.Fatal("负载长度异常")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("同参数负载必须确定性一致")
		}
	}
	// 重复素材种子固定：同帧号的 duplicate 负载与 normal 负载不同，
	// 但两个不同帧号的 duplicate 负载彼此相同（用于跨源指纹去重）。
	dupA := gradientPayload(0, true)
	dupB := gradientPayload(99, true)
	for i := range dupA {
		if dupA[i] != dupB[i] {
			t.Fatal("重复素材应内容相同以触发指纹去重")
		}
	}
	norm := gradientPayload(0, false)
	if string(norm) == string(dupA) {
		t.Fatal("重复素材应与普通帧区分，避免全部误并")
	}
}
