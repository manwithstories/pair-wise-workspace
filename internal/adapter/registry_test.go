package adapter

import (
	"testing"

	"archivemedia/internal/model"
)

type fakeParser struct {
	name    string
	version string
	caps    []Capability
	probe   bool
}

func (f *fakeParser) Name() string             { return f.name }
func (f *fakeParser) Version() string          { return f.version }
func (f *fakeParser) Capabilities() []Capability { return f.caps }
func (f *fakeParser) Probe(string) (bool, error) { return f.probe, nil }
func (f *fakeParser) Parse(string, ParseOptions) (*ParsedMedia, error) {
	return &ParsedMedia{AdapterName: f.name, AdapterVersion: f.version}, nil
}

func newReg(t *testing.T) {
	t.Helper()
	globalRegistry = &registry{parsers: map[string]Parser{}}
}

func TestRegisterAndLatest(t *testing.T) {
	newReg(t)
	MustRegister(&fakeParser{name: "arc1", version: "1.0.0"})
	if err := Register(&fakeParser{name: "arc1", version: "1.0.0"}); err == nil {
		t.Fatal("重复注册同名同版本必须失败")
	}
	MustRegister(&fakeParser{name: "arc1", version: "1.2.0"})
	MustRegister(&fakeParser{name: "astiav", version: "1.0.0"})
	p, ok := Latest("arc1")
	if !ok || p.Version() != "1.2.0" {
		t.Fatalf("Latest arc1=%v ok=%v", p, ok)
	}
	if _, ok := Get("arc1", "9.9.9"); ok {
		t.Fatal("不存在的版本不应命中")
	}
	if got := len(List()); got != 3 {
		t.Fatalf("List 数量=%d", got)
	}
}

func TestNegotiateCaps(t *testing.T) {
	newReg(t)
	old := &fakeParser{name: "arc1", version: "1.0.0", caps: []Capability{CapDemux}, probe: true}
	new := &fakeParser{name: "arc1", version: "2.0.0", caps: []Capability{CapDemux, CapDecode, CapWrapExpand}, probe: true}
	MustRegister(old)
	MustRegister(new)
	// 需要回绕展开能力：仅 2.0.0 满足
	p, err := Negotiate("x.arc", []Capability{CapDemux, CapWrapExpand})
	if err != nil || p.Version() != "2.0.0" {
		t.Fatalf("negotiate=%v err=%v", p, err)
	}
	// 全部适配器都不具备的能力 -> 不支持格式错误
	_, err = Negotiate("x.arc", []Capability{Capability("time_travel")})
	if model.KindOf(err) != model.KindUnsupportedFormat {
		t.Fatalf("期望 unsupported，got %v", err)
	}
}

func TestSemver(t *testing.T) {
	cases := []struct {
		a, b string
		less bool
	}{
		{"1.0.0", "1.0.1", true},
		{"v1.2.9", "v1.2.10", true},
		{"2.0.0", "1.9.9", false},
		{"1.10.0", "1.9.0", false},
	}
	for _, c := range cases {
		if got := semverLess(c.a, c.b); got != c.less {
			t.Fatalf("semverLess(%s,%s)=%v want %v", c.a, c.b, got, c.less)
		}
	}
}

func TestDefaultLimits(t *testing.T) {
	d := DefaultLimits()
	if d.MaxTracks <= 0 || d.MaxConcurrency <= 0 || d.MaxFileBytes <= 0 || d.MaxDurationUs <= 0 {
		t.Fatalf("默认限制必须为正值: %+v", d)
	}
}
