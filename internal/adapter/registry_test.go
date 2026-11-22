package adapter

import (
	"context"
	"strings"
	"testing"

	"mediacheck/internal/model"
)

type stubParser struct{ family, version string }

func (s stubParser) Family() string  { return s.family }
func (s stubParser) Version() string { return s.version }
func (s stubParser) Parse(context.Context, ParseInput) (*ParseResult, error) {
	return &ParseResult{}, nil
}

func TestRegistryVersionSelection(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubParser{"mp4", "1.0.0"}, []string{"ipcm", "mp4a"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(stubParser{"mp4", "2.1.0"}, []string{"ipcm"}); err != nil {
		t.Fatal(err)
	}
	// 重复注册同版本被拒绝。
	if err := r.Register(stubParser{"mp4", "1.0.0"}, nil); err == nil {
		t.Fatal("duplicate registration must fail")
	}
	// 最新 major。
	p, err := r.Lookup("mp4", 0)
	if err != nil || p.Version() != "2.1.0" {
		t.Fatalf("latest lookup = %v, %v", p, err)
	}
	// 兼容 major 上限。
	p, err = r.Lookup("mp4", 1)
	if err != nil || p.Version() != "1.0.0" {
		t.Fatalf("pinned lookup = %v, %v", p, err)
	}
	if _, err = r.Lookup("mkv", 0); err == nil {
		t.Fatal("unknown family must fail")
	}
}

func TestRegistryCodecClaims(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubParser{"rtp", "1.2.0"}, []string{"opus"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(stubParser{"mp4", "1.0.0"}, []string{"ipcm"}); err != nil {
		t.Fatal(err)
	}
	// 同一 codec 被第二个 family 声明必须失败，避免绕过统一内核。
	err := r.Register(stubParser{"mp4", "2.0.0"}, []string{"opus"})
	if err == nil || !strings.Contains(err.Error(), "opus") {
		t.Fatalf("cross-family codec claim must fail, got %v", err)
	}
	if err := r.RequireCodec("opus"); err != nil {
		t.Fatal(err)
	}
	err = r.RequireCodec("av01")
	me, ok := model.AsMediaError(err)
	if !ok || me.Category != model.ErrUnknownCodec {
		t.Fatalf("unregistered codec must yield structured error, got %v", err)
	}
	if fams := r.Families(); len(fams) != 2 || fams[0] != "mp4" {
		t.Fatalf("families snapshot = %v", fams)
	}
}

func TestCheckInputLimits(t *testing.T) {
	in := ParseInput{
		InitSegment:   make([]byte, 10),
		Fragments:     [][]byte{make([]byte, 5), make([]byte, 7)},
		MaxBoxSize:    6,
		MaxInputBytes: 100,
	}
	err := CheckInputLimits(in)
	me, ok := model.AsMediaError(err)
	if !ok || me.Category != model.ErrOversizeInput {
		t.Fatalf("oversize fragment must be rejected: %v", err)
	}
	in.MaxBoxSize = 100
	in.MaxInputBytes = 20
	if err = CheckInputLimits(in); err == nil {
		t.Fatal("total oversize must be rejected")
	}
	in.MaxInputBytes = 100
	if err = CheckInputLimits(in); err != nil {
		t.Fatalf("within limits must pass: %v", err)
	}
}

func TestSafeOutputPath(t *testing.T) {
	base := t.TempDir()
	good, err := SafeOutputPath(base, "frag/init.mp4")
	if err != nil || !strings.HasSuffix(good, "frag/init.mp4") {
		t.Fatalf("safe path = %q, %v", good, err)
	}
	for _, bad := range []string{"../escape.mp4", "/etc/passwd", "a/../../b"} {
		if _, err := SafeOutputPath(base, bad); err == nil {
			t.Fatalf("path %q must be rejected", bad)
		} else if me, ok := model.AsMediaError(err); !ok || me.Category != model.ErrPathViolation {
			t.Fatalf("path %q must yield path error, got %v", bad, err)
		}
	}
}
