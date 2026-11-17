package report_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"hdrcheck/internal/fixture"
	"hdrcheck/internal/garc"
	"hdrcheck/internal/hdr"
	"hdrcheck/internal/report"
	"hdrcheck/internal/validate"
)

func buildEnvelope(t *testing.T, scenario string) report.Envelope {
	t.Helper()
	for _, sc := range fixture.Scenarios() {
		if sc.Name != scenario {
			continue
		}
		b, _ := sc.Build()
		data, _ := b.Build()
		f, err := garc.Parse(data)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		c, _ := hdr.ParseContainer(f)
		sei, _ := hdr.ParseSEI(f)
		rep := validate.Build(f, c, sei, "abc123")
		return report.BuildEnvelope("/secret/dir/"+scenario, rep)
	}
	t.Fatalf("scenario %s missing", scenario)
	return report.Envelope{}
}

func TestSafeBaseNameHidesDirectory(t *testing.T) {
	env := buildEnvelope(t, "hdr-mixed.garc")
	if env.Input.Name != "hdr-mixed.garc" {
		t.Errorf("input name leaks path: %q", env.Input.Name)
	}
	var buf bytes.Buffer
	if err := report.WriteText(&buf, env); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "secret") {
		t.Error("terminal output leaks absolute directory")
	}
}

func TestTextSummaryContainsRequiredFields(t *testing.T) {
	env := buildEnvelope(t, "hdr-mixed.garc")
	var buf bytes.Buffer
	_ = report.WriteText(&buf, env)
	out := buf.String()
	for _, want := range []string{
		"input_sha256", "max_content_light", "video_track_count",
		"conflict_count", "missing_count", "error_count", "source=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q", want)
		}
	}
}

func TestJSONValidAndSorted(t *testing.T) {
	env := buildEnvelope(t, "hdr-mixed.garc")
	var first bytes.Buffer
	if err := report.WriteJSON(&first, env); err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(first.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// 重新构建并序列化，结果须字节级一致。
	env2 := buildEnvelope(t, "hdr-mixed.garc")
	var second bytes.Buffer
	_ = report.WriteJSON(&second, env2)
	if first.String() != second.String() {
		t.Error("JSON serialization not deterministic")
	}
	// 轨道按 track_id 升序。
	for i := 1; i < len(env.Tracks); i++ {
		if env.Tracks[i-1].TrackID > env.Tracks[i].TrackID {
			t.Error("tracks not sorted by id")
		}
	}
}

func TestCleanFileReportsNoInconsistencies(t *testing.T) {
	env := buildEnvelope(t, "hdr-valid.garc")
	var buf bytes.Buffer
	_ = report.WriteText(&buf, env)
	if !strings.Contains(buf.String(), "no inconsistencies detected") {
		t.Error("valid file should report no inconsistencies")
	}
}
