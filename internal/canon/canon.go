package canon

import (
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"unicode"

	"github.com/cespare/xxhash/v2"
	"golang.org/x/text/unicode/norm"
)

// ResolveURL resolves relative and protocol-relative links against base.
func ResolveURL(base *url.URL, raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty URL")
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse URL %q: %w", raw, err)
	}
	if base == nil {
		if ref.IsAbs() {
			return ref, nil
		}
		return nil, fmt.Errorf("relative URL %q without base", raw)
	}
	u := base.ResolveReference(ref)
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
	return u, nil
}

// CanonicalURL returns a stable URL identity. Fragments, default ports,
// credentials, common tracking parameters and insignificant trailing slashes
// are removed. Remaining query parameters are sorted.
func CanonicalURL(u *url.URL, removeQueryParams []string) string {
	if u == nil {
		return ""
	}
	out := *u
	out.Scheme = strings.ToLower(out.Scheme)
	out.Host = strings.ToLower(out.Host)
	out.Fragment = ""
	out.User = nil
	out.Opaque = ""

	host := out.Hostname()
	port := out.Port()
	if (out.Scheme == "http" && port == "80") || (out.Scheme == "https" && port == "443") {
		port = ""
	}
	if port == "" {
		out.Host = host
	} else {
		out.Host = host + ":" + port
	}

	cleanPath := path.Clean("/" + strings.TrimPrefix(out.Path, "/"))
	if cleanPath == "." || cleanPath == "" {
		cleanPath = "/"
	}
	cleanPath = strings.TrimSuffix(cleanPath, "/")
	if cleanPath == "" {
		cleanPath = "/"
	}
	out.Path = cleanPath
	out.RawPath = ""

	drop := make(map[string]bool, len(removeQueryParams))
	for _, key := range removeQueryParams {
		drop[strings.ToLower(strings.TrimSpace(key))] = true
	}
	values := out.Query()
	for key := range values {
		if drop[strings.ToLower(key)] {
			values.Del(key)
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		vs := values[key]
		sort.Strings(vs)
		for _, value := range vs {
			if b.Len() > 0 {
				b.WriteByte('&')
			}
			b.WriteString(url.QueryEscape(key))
			b.WriteByte('=')
			b.WriteString(url.QueryEscape(value))
		}
	}
	out.RawQuery = b.String()
	return strings.TrimRight(out.String(), "/")
}

func CanonicalizeURL(raw string, base *url.URL, removeQueryParams []string) (string, error) {
	u, err := ResolveURL(base, raw)
	if err != nil {
		return "", err
	}
	return CanonicalURL(u, removeQueryParams), nil
}

// NormalizeTitle makes title matching tolerant of full-width characters,
// zero-width characters, case differences and arbitrary whitespace.
func NormalizeTitle(s string) string {
	s = norm.NFKC.String(s)
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == '\u200b' || r == '\u200c' || r == '\u200d' || r == '\ufeff':
			continue
		case unicode.IsSpace(r):
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = true
		default:
			space = false
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return strings.TrimSpace(b.String())
}

func NormalizeBusinessPart(s string) string {
	s = norm.NFKC.String(s)
	var b strings.Builder
	for _, r := range s {
		if unicode.IsSpace(r) || r == '\u200b' || r == '\u200c' || r == '\u200d' || r == '\ufeff' {
			continue
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

// BusinessKey is the cross-site identity: project number plus bid section.
func BusinessKey(projectNo, section string) string {
	project := NormalizeBusinessPart(projectNo)
	sect := NormalizeBusinessPart(section)
	if project == "" {
		return ""
	}
	if sect == "" {
		return project
	}
	return project + "|" + sect
}

func NormalizeBody(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = norm.NFKC.String(s)
	s = strings.ReplaceAll(s, "\u00a0", " ")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// Fingerprint identifies a content revision independently of the source URL.
// Publication time is deliberately excluded: a corrected timestamp alone is
// not a new revision.
func Fingerprint(title, body, projectNo, section string) string {
	payload := strings.Join([]string{
		NormalizeBusinessPart(projectNo),
		NormalizeBusinessPart(section),
		NormalizeTitle(title),
		NormalizeBody(body),
	}, "\x00")
	sum := xxhash.Sum64String(payload)
	return fmt.Sprintf("%016x", sum)
}

func TruncateRunes(s string, limit int) string {
	if limit <= 0 {
		return s
	}
	rs := []rune(s)
	if len(rs) <= limit {
		return s
	}
	return string(rs[:limit])
}
