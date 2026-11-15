package manifest

import (
	"strings"
	"testing"

	"dashpack/internal/errs"
)

func TestParseDurationValid(t *testing.T) {
	cases := map[string]int64{
		"PT0S":         0,
		"PT47S":        47_000_000_000,
		"PT1.5S":       1_500_000_000,
		"PT2M":         120_000_000_000,
		"PT1H2M3.25S":  3723_250_000_000,
		"PT23H59M59S":  86399_000_000_000,
		"P1D":          86400_000_000_000,
		"PT0.000000001S": 1,
	}
	for in, want := range cases {
		got, err := ParseDuration(in)
		if err != nil {
			t.Fatalf("ParseDuration(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseDuration(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseDurationInvalid(t *testing.T) {
	for _, in := range []string{"", "-PT1S", "PT25H", "P1DT1S", "PT1X", "abc", "P", "PT", "PT1.S"} {
		if _, err := ParseDuration(in); err == nil {
			t.Fatalf("ParseDuration(%q) should fail", in)
		} else if ce, ok := err.(*errs.Coded); !ok || ce.Code != errs.EDuration {
			t.Fatalf("ParseDuration(%q) err = %v, want E_DURATION", in, err)
		}
	}
}

func TestFormatDurationRoundTrip(t *testing.T) {
	for _, ns := range []int64{0, 1, 1_500_000_000, 47_000_000_000, 86399_000_000_000} {
		s := FormatDuration(ns)
		got, err := ParseDuration(s)
		if err != nil || got != ns {
			t.Fatalf("round trip %d -> %q -> %d, %v", ns, s, got, err)
		}
	}
}

func TestExpandTemplate(t *testing.T) {
	vars := map[string]any{"RepresentationID": "v1", "Number": int64(7), "Time": int64(360000)}
	got, err := ExpandTemplate("v1/seg_$Number%05d$.m4s", vars)
	if err != nil || got != "v1/seg_00007.m4s" {
		t.Fatalf("got %q, %v", got, err)
	}
	got, err = ExpandTemplate("$RepresentationID$/init.mp4", vars)
	if err != nil || got != "v1/init.mp4" {
		t.Fatalf("got %q, %v", got, err)
	}
	got, err = ExpandTemplate("cost$$_$Number$.m4s", vars)
	if err != nil || got != "cost$_7.m4s" {
		t.Fatalf("got %q, %v", got, err)
	}
	got, err = ExpandTemplate("t$Time$.m4s", vars)
	if err != nil || got != "t360000.m4s" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestExpandTemplateInvalid(t *testing.T) {
	vars := map[string]any{"RepresentationID": "v1", "Number": int64(1)}
	for _, tmpl := range []string{
		"seg_$Number%xd$.m4s",   // 非法宽度
		"seg_$Number%00d$.m4s",  // 零宽度
		"seg_$Number%99d$.m4s",  // 过宽
		"seg_$Number.m4s",       // 未闭合
		"seg_$Nope$.m4s",        // 未知变量
		"seg_$RepresentationID%05d$.m4s", // 字符串变量禁宽度
	} {
		if _, err := ExpandTemplate(tmpl, vars); err == nil {
			t.Fatalf("ExpandTemplate(%q) should fail", tmpl)
		} else if ce, ok := err.(*errs.Coded); !ok || ce.Code != errs.ETemplate {
			t.Fatalf("ExpandTemplate(%q) err = %v, want E_TEMPLATE", tmpl, err)
		}
	}
	// 变量注入路径分隔符
	if _, err := ExpandTemplate("$RepresentationID$/x.m4s",
		map[string]any{"RepresentationID": "../evil"}); err == nil ||
		!strings.Contains(err.Error(), errs.ETemplate) {
		t.Fatalf("path separator injection should fail with E_TEMPLATE, got %v", err)
	}
}

func TestValidateDuplicateID(t *testing.T) {
	rep := func(id string) Representation {
		return Representation{ID: id, Bandwidth: 1, Codecs: "x",
			Template: SegmentTemplate{Timescale: 90000, Duration: 180000,
				StartNumber: 1, Media: "m", Initialization: "i"}}
	}
	m := &MPD{Periods: []Period{{AdaptationSets: []AdaptationSet{
		{ContentType: "video", Representations: []Representation{rep("a"), rep("a")}},
	}}}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate id should fail, got %v", err)
	}
	m.Periods[0].AdaptationSets[0].Representations[1].ID = "b"
	if err := m.Validate(); err != nil {
		t.Fatalf("valid model: %v", err)
	}
	m.Periods[0].AdaptationSets[0].ContentType = ""
	if err := m.Validate(); err == nil {
		t.Fatal("missing contentType should fail")
	}
}
