package fixture

import (
	"context"
	"testing"

	"mediacheck/internal/adapter"
	mp4adapter "mediacheck/internal/adapter/mp4"
	"mediacheck/internal/model"
)

func TestGenerateNormalBundleParses(t *testing.T) {
	b := Generate(7, 48000, model.LayoutStereo, 4, InjectNone)
	if len(b.Fragments) != 4 {
		t.Fatalf("frags = %d", len(b.Fragments))
	}
	if len(b.GapSeqs) != 1 || b.GapSeqs[0] != 3 {
		t.Fatalf("gap seqs = %v", b.GapSeqs)
	}
	p := mp4adapter.New()
	res, err := p.Parse(context.Background(), adapter.ParseInput{InitSegment: b.InitSegment, Fragments: b.Fragments})
	if err != nil {
		t.Fatal(err)
	}
	if res.Tracks[0].Layout != model.LayoutStereo {
		t.Fatalf("layout = %q", res.Tracks[0].Layout)
	}
}

func TestInjectedCorruptionsPropagate(t *testing.T) {
	cases := []struct {
		inj  Injection
		want model.ErrorCategory
	}{
		{InjectMissingMFHD, model.ErrMissingMFHD},
		{InjectTruncatedBox, model.ErrTruncatedBox},
	}
	p := mp4adapter.New()
	for _, tc := range cases {
		b := Generate(7, 48000, model.LayoutStereo, 3, tc.inj)
		_, err := p.Parse(context.Background(), adapter.ParseInput{InitSegment: b.InitSegment, Fragments: b.Fragments})
		me, ok := model.AsMediaError(err)
		if !ok || me.Category != tc.want {
			t.Fatalf("%s: want %s, got %v", tc.inj, tc.want, err)
		}
	}
}

func TestUnknownLayoutBundle(t *testing.T) {
	b := Generate(7, 48000, model.LayoutStereo, 2, InjectUnknownLayout)
	p := mp4adapter.New()
	res, err := p.Parse(context.Background(), adapter.ParseInput{InitSegment: b.InitSegment, Fragments: b.Fragments})
	if err != nil {
		t.Fatal(err)
	}
	if res.Tracks[0].Layout != "" {
		t.Fatalf("unknown layout must parse to empty layout, got %q", res.Tracks[0].Layout)
	}
}

func TestTFDTRegressionBundleStructure(t *testing.T) {
	// tfdt 回退素材本身可解析，错误由时钟内核按策略拒绝（跨模块传播在 core 测试覆盖）。
	b := Generate(7, 48000, model.LayoutStereo, 3, InjectTFDTRegression)
	p := mp4adapter.New()
	res, err := p.Parse(context.Background(), adapter.ParseInput{InitSegment: b.InitSegment, Fragments: b.Fragments})
	if err != nil {
		t.Fatalf("tfdt regression should parse, clock kernel must reject: %v", err)
	}
	if res.Fragments[1].BaseMediaDecodeTime != 0 {
		t.Fatalf("second fragment tfdt should regress to 0, got %d", res.Fragments[1].BaseMediaDecodeTime)
	}
}

func TestDeterministicGeneration(t *testing.T) {
	a := Generate(7, 44100, model.Layout51, 3, InjectNone)
	c := Generate(7, 44100, model.Layout51, 3, InjectNone)
	if len(a.Fragments) != len(c.Fragments) {
		t.Fatal("fragment count mismatch")
	}
	for i := range a.Fragments {
		if string(a.Fragments[i]) != string(c.Fragments[i]) {
			t.Fatal("same seed must produce identical bytes")
		}
	}
}
