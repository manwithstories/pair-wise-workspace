package canon

import (
	"net/url"
	"strings"
	"testing"
)

func TestCanonicalizeRelativeAndProtocolRelative(t *testing.T) {
	base, _ := url.Parse("HTTPS://Example.COM:443/list/page-1?utm_source=a")
	tests := []struct {
		raw  string
		want string
	}{
		{"/ann/1?b=2&a=1#part", "https://example.com/ann/1?a=1&b=2"},
		{"//CDN.example.com/notice/1/", "https://cdn.example.com/notice/1"},
		{"../notice/2?utm_content=x&from=list", "https://example.com/notice/2"},
		{"http://example.com:80/a/", "http://example.com/a"},
	}
	for _, tc := range tests {
		got, err := CanonicalizeURL(tc.raw, base, []string{"utm_source", "utm_content", "from"})
		if err != nil {
			t.Fatalf("%q: %v", tc.raw, err)
		}
		if got != tc.want {
			t.Errorf("%q: got %q want %q", tc.raw, got, tc.want)
		}
	}
}

func TestNormalizeTitleAndBusinessKey(t *testing.T) {
	if got := NormalizeTitle("  Ｐ－００１　 招标\u200b公告  "); got != "p-001 招标公告" {
		t.Fatalf("normalize title = %q", got)
	}
	if got := BusinessKey(" p-001 ", " 一 标 段 "); got != "P-001|一标段" {
		t.Fatalf("business key = %q", got)
	}
	if BusinessKey("", "x") != "" {
		t.Fatal("empty project number must not produce a key")
	}
}

func TestFingerprintDetectsContentChange(t *testing.T) {
	h1 := Fingerprint("公告", "正文一", "P-1", "S1")
	h2 := Fingerprint("公告", "正文二", "P-1", "S1")
	h3 := Fingerprint(" 公告 ", "正文一\r\n", " p-1 ", " s1 ")
	if h1 == h2 {
		t.Fatal("content change was not detected")
	}
	if h1 != h3 {
		t.Fatal("normalization should make equivalent content fingerprint equal")
	}
	if strings.TrimSpace(h1) == "" {
		t.Fatal("empty fingerprint")
	}
}
