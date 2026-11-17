// Package tests 集成测试：驱动 fixture 生成 → inspect → validate 全链路，
// 验证输出字段、数量级、稳定 JSON 与四类退出码契约。
package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"hdrcheck/internal/app"
	"hdrcheck/internal/fixture"
	"hdrcheck/internal/garc"
	"hdrcheck/internal/hdr"
	"hdrcheck/internal/report"
)

var shaRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestFullChainMixedAcceptance(t *testing.T) {
	dir := t.TempDir()
	names, err := fixture.GenerateAll(dir)
	if err != nil || len(names) != 10 {
		t.Fatalf("GenerateAll: names=%d err=%v", len(names), err)
	}
	mixedPath := filepath.Join(dir, "hdr-mixed.garc")
	res, err := app.Run(mixedPath)
	if err != nil {
		t.Fatalf("app.Run mixed: %v", err)
	}
	rep := res.Report

	// 验收清单字段。
	if rep.Summary.VideoTrackCount != 3 {
		t.Errorf("video_track_count=%d want 3", rep.Summary.VideoTrackCount)
	}
	if rep.Summary.ConflictCount != 1 {
		t.Errorf("conflict_count=%d want 1", rep.Summary.ConflictCount)
	}
	if rep.Summary.MissingCount != 1 {
		t.Errorf("missing_count=%d want 1", rep.Summary.MissingCount)
	}
	if rep.Summary.ErrorCount != 0 && rep.Summary.ErrorCount != 1 {
		t.Errorf("error_count=%d want 0 or 1", rep.Summary.ErrorCount)
	}
	if !shaRe.MatchString(rep.Summary.InputSHA256) {
		t.Errorf("input_sha256 not 64-hex: %q", rep.Summary.InputSHA256)
	}

	// mastering_display.present=true 且 source=container。
	tr1 := rep.Tracks[0]
	if !tr1.Mastering.Present || tr1.Mastering.Source != hdr.SourceContainer {
		t.Errorf("mastering present/source wrong: %+v", tr1.Mastering)
	}
	// max_content_light_level.nits=1000。
	if tr1.MaxCLL.Nits != 1000 || !tr1.MaxCLL.Present {
		t.Errorf("max_content_light_level nits=%d want 1000", tr1.MaxCLL.Nits)
	}

	// 每个问题项都有 inconsistency_code（Code 非空），按轨可列。
	for _, tr := range rep.Tracks {
		for _, is := range tr.Issues {
			if is.Code == "" {
				t.Error("issue missing inconsistency_code")
			}
		}
	}
}

func TestJSONReportStableAndComplete(t *testing.T) {
	dir := t.TempDir()
	_, _ = fixture.GenerateAll(dir)
	res, err := app.Run(filepath.Join(dir, "hdr-mixed.garc"))
	if err != nil {
		t.Fatal(err)
	}
	env := report.BuildEnvelope("hdr-mixed.garc", res.Report)
	path := filepath.Join(dir, "inspect.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := report.WriteJSON(f, env); err != nil {
		t.Fatal(err)
	}
	f.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("JSON invalid: %v", err)
	}
	// 再写一次，字节级一致 → 稳定排序。
	res2, _ := app.Run(filepath.Join(dir, "hdr-mixed.garc"))
	path2 := filepath.Join(dir, "inspect2.json")
	f2, _ := os.Create(path2)
	_ = report.WriteJSON(f2, report.BuildEnvelope("hdr-mixed.garc", res2.Report))
	f2.Close()
	raw2, _ := os.ReadFile(path2)
	if string(raw) != string(raw2) {
		t.Error("JSON output not deterministic across runs")
	}
}

func TestExitCodeContract(t *testing.T) {
	dir := t.TempDir()
	_, _ = fixture.GenerateAll(dir)

	cases := []struct {
		file   string
		strict bool
		want   int
	}{
		{"hdr-valid.garc", true, app.ExitOK},
		{"hdr-valid.garc", false, app.ExitOK},
		{"hdr-mixed.garc", true, app.ExitBusiness},
		{"hdr-missing-fields.garc", true, app.ExitBusiness},
		{"hdr-mixed.garc", false, app.ExitOK}, // 审计模式 warning 不阻断
		{"hdr-inherited.garc", true, app.ExitOK},
	}
	for _, tc := range cases {
		res, err := app.Run(filepath.Join(dir, tc.file))
		if err != nil {
			t.Errorf("%s: unexpected run error %v", tc.file, err)
			continue
		}
		if got := app.DecideExitCode(res.Report, tc.strict); got != tc.want {
			t.Errorf("%s strict=%v exit=%d want %d", tc.file, tc.strict, got, tc.want)
		}
	}

	// 结构损坏在头解析阶段即返回 *garc.Error（退出码 3 类）。
	for _, corrupt := range []string{
		"hdr-truncated.garc", "hdr-unknown-version.garc",
		"hdr-length-mismatch.garc",
	} {
		if _, err := app.Run(filepath.Join(dir, corrupt)); err == nil {
			t.Errorf("%s: expected corrupt-class error", corrupt)
		} else if !app.IsCorruptError(err) {
			t.Errorf("%s: error not classified corrupt: %v", corrupt, err)
		}
	}

	// 内容级损坏（重复字段/时间重叠）头可解析，但报告含 error 级问题 → 退出 3。
	for _, contentCorrupt := range []string{"hdr-duplicate.garc", "hdr-overlap.garc"} {
		res, err := app.Run(filepath.Join(dir, contentCorrupt))
		if err != nil {
			t.Fatalf("%s header should parse: %v", contentCorrupt, err)
		}
		if res.Report.Summary.ErrorCount == 0 {
			t.Errorf("%s: expected at least one error-level issue", contentCorrupt)
		}
		if got := app.DecideExitCode(res.Report, false); got != app.ExitCorruptOrArgs {
			t.Errorf("%s exit=%d want %d", contentCorrupt, got, app.ExitCorruptOrArgs)
		}
	}

	// badchecksum 能通过头解析，但 SEI 阶段产生 error 级问题 → 退出 3。
	res, err := app.Run(filepath.Join(dir, "hdr-badchecksum.garc"))
	if err != nil {
		t.Fatalf("badchecksum should parse header: %v", err)
	}
	if got := app.DecideExitCode(res.Report, false); got != app.ExitCorruptOrArgs {
		t.Errorf("badchecksum exit=%d want %d", got, app.ExitCorruptOrArgs)
	}
}

func TestCorruptionIssueCodes(t *testing.T) {
	dir := t.TempDir()
	_, _ = fixture.GenerateAll(dir)
	wantCodes := map[string]string{
		"hdr-duplicate.garc": "DUPLICATE_FIELD",
		"hdr-overlap.garc":   "RECORDS_OVERLAP",
	}
	for file, code := range wantCodes {
		data, _ := os.ReadFile(filepath.Join(dir, file))
		f, err := garc.Parse(data)
		if err != nil {
			// duplicate/overlap 头仍合法，预期可解析。
			t.Fatalf("%s parse: %v", file, err)
		}
		c, _ := hdr.ParseContainer(f)
		sei, _ := hdr.ParseSEI(f)
		all := append(append([]hdr.Issue{}, c.Issues...), sei.Issues...)
		found := false
		for _, is := range all {
			if is.Code == code {
				found = true
			}
		}
		if !found {
			t.Errorf("%s missing expected code %s (got %+v)", file, code, all)
		}
	}
}
