package extract

import (
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"

	"crawler/internal/config"
)

func testSite(maxBody int) *config.Site {
	return &config.Site{
		Name: "fixture", Concurrency: 1, RatePerSecond: 1, Burst: 1,
		Timeout: config.Duration(1e9), MaxPages: 10, MaxBodyRunes: maxBody,
		TimeLayouts: []string{"2006-01-02 15:04:05"},
		Selectors: config.Selectors{
			Item: ".item", Title: ".title", Link: ".title", ProjectNo: ".project",
			Section: ".section", PublishTime: ".publish", Body: ".body",
			Next: ".next a", PageNum: ".page",
		},
	}
}

func TestParsePageGBKRelativeAndTruncation(t *testing.T) {
	html := `<!doctype html><html><head><meta charset="gbk"></head><body>
<span class="page">2</span>
<li class="item">
 <a class="title" href="../ann/1?utm_source=x">招标公告 P-001</a>
 <span class="project">P-001</span><span class="section">一标段</span>
 <span class="publish">2026-11-12 09:00:00</span>
 <div class="body">` + strings.Repeat("很长的正文", 20) + `</div>
</li>
<li class="item"><a class="title" href="//cdn.example.com/ann/2"></a>
 <span class="project">P-002</span><span class="publish">2026-11-12 09:01:00</span><div class="body">坏标题</div>
</li>
<div class="next"><a href="/list/page-3?page=3">下一页</a></div>
</body></html>`
	gbk, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(html))
	if err != nil {
		t.Fatal(err)
	}
	site := testSite(25)
	page, err := ParsePage(site, "http://example.com/list/page-2", gbk, "text/html; charset=gbk")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Candidates) != 2 {
		t.Fatalf("candidates=%d", len(page.Candidates))
	}
	first := page.Candidates[0]
	if first.ProjectNo != "P-001" || first.Section != "一标段" || first.PublishTime.IsZero() {
		t.Fatalf("bad extraction: %+v", first)
	}
	if len([]rune(first.Body)) > 25 {
		t.Fatalf("body not truncated: %d", len([]rune(first.Body)))
	}
	if page.Candidates[1].ParseError == nil {
		t.Fatal("empty title should be a parse error")
	}
	if page.PageNum != 2 || page.NextRaw != "/list/page-3?page=3" {
		t.Fatalf("pagination fields: num=%d next=%q", page.PageNum, page.NextRaw)
	}
}

func TestParsePageBrokenSelector(t *testing.T) {
	site := testSite(100)
	_, err := ParsePage(site, "http://example.com/list", []byte(`<html><body><div>empty</div></body></html>`), "text/html")
	if err == nil {
		t.Fatal("expected parse failure for missing item selector")
	}
}
