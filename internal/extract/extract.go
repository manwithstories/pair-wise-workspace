package extract

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html/charset"

	"github.com/PuerkitoBio/goquery"

	"crawler/internal/canon"
	"crawler/internal/config"
)

type Candidate struct {
	RawURL      string
	Title       string
	ProjectNo   string
	Section     string
	PublishRaw  string
	PublishTime time.Time
	Body        string
	Status      string
	ParseError  error
	PageURL     string
	PageNum     int
}

type Page struct {
	URL        string
	Candidates []Candidate
	NextRaw    string
	PageNum    int
}

// ParsePage applies the site's list-page selectors to a downloaded document.
func ParsePage(site *config.Site, pageURL string, body []byte, contentType string) (*Page, error) {
	doc, err := decodeDocument(body, contentType)
	if err != nil {
		return nil, err
	}
	items := doc.Find(site.Selectors.Item)
	if items.Length() == 0 {
		return nil, fmt.Errorf("parse failed: item selector %q matched nothing", site.Selectors.Item)
	}
	page := &Page{URL: pageURL}
	page.PageNum = extractPageNum(doc.Selection, site.Selectors.PageNum)
	page.NextRaw = findLinkOrText(doc.Selection, site.Selectors.Next)
	items.Each(func(i int, item *goquery.Selection) {
		c := extractCandidate(site, pageURL, item, page.PageNum)
		if c.ParseError == nil && (c.PublishRaw != "" || !site.Detail) {
			c.PublishTime, c.ParseError = parsePublishTime(c.PublishRaw, site.TimeLayouts)
		}
		c.Body = canon.TruncateRunes(canon.NormalizeBody(c.Body), site.MaxBodyRunes)
		page.Candidates = append(page.Candidates, c)
	})
	return page, nil
}

// ParseDetail applies the same field selectors against a detail document.
func ParseDetail(site *config.Site, pageURL string, body []byte, contentType string) (*Candidate, error) {
	doc, err := decodeDocument(body, contentType)
	if err != nil {
		return nil, err
	}
	root := doc.Selection
	c := Candidate{PageURL: pageURL, RawURL: pageURL}
	c.Title = collapse(findText(root, site.Selectors.Title))
	c.ProjectNo = collapse(findText(root, site.Selectors.ProjectNo))
	c.Section = collapse(findText(root, site.Selectors.Section))
	c.PublishRaw = collapse(findText(root, site.Selectors.PublishTime))
	c.Body = canon.TruncateRunes(canon.NormalizeBody(findText(root, site.Selectors.Body)), site.MaxBodyRunes)
	c.Status = collapse(findText(root, site.Selectors.Status))
	if c.Title == "" {
		return nil, fmt.Errorf("parse failed: empty title")
	}
	if c.ProjectNo == "" {
		return nil, fmt.Errorf("parse failed: empty project number")
	}
	c.PublishTime, err = parsePublishTime(c.PublishRaw, site.TimeLayouts)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func extractCandidate(site *config.Site, pageURL string, item *goquery.Selection, pageNum int) Candidate {
	c := Candidate{PageURL: pageURL, PageNum: pageNum}
	c.Title = collapse(findText(item, site.Selectors.Title))
	c.ProjectNo = collapse(findText(item, site.Selectors.ProjectNo))
	c.Section = collapse(findText(item, site.Selectors.Section))
	c.PublishRaw = collapse(findText(item, site.Selectors.PublishTime))
	c.Body = canon.TruncateRunes(findText(item, site.Selectors.Body), site.MaxBodyRunes)
	c.Status = collapse(findText(item, site.Selectors.Status))

	linkSelector := site.Selectors.Link
	if linkSelector == "" {
		linkSelector = site.Selectors.Title
	}
	c.RawURL = findLinkOrText(item, linkSelector)
	if c.RawURL == "" {
		if value, ok := item.Attr("href"); ok && site.Selectors.Link == "" {
			c.RawURL = value
		}
	}
	if c.Title == "" {
		c.ParseError = fmt.Errorf("parse failed: empty title")
		return c
	}
	if c.RawURL == "" {
		c.ParseError = fmt.Errorf("parse failed: missing announcement link")
	}
	if c.ProjectNo == "" && !site.Detail {
		c.ParseError = fmt.Errorf("parse failed: empty project number")
	}
	return c
}

func parsePublishTime(raw string, layouts []string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("parse failed: empty publish time")
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), nil
		}
		loc := time.Local
		if t, err := time.ParseInLocation(layout, raw, loc); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("parse failed: unsupported publish time %q", raw)
}

func decodeDocument(body []byte, contentType string) (*goquery.Document, error) {
	reader, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		return nil, fmt.Errorf("decode HTML: %w", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read decoded HTML: %w", err)
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(decoded))
	if err != nil {
		return nil, fmt.Errorf("parse HTML: %w", err)
	}
	return doc, nil
}

var pageNumberRE = regexp.MustCompile(`[0-9]+`)

func extractPageNum(root *goquery.Selection, selector string) int {
	raw := collapse(findLinkOrText(root, selector))
	if raw == "" {
		return 0
	}
	for _, token := range pageNumberRE.FindAllString(raw, -1) {
		if n, err := strconv.Atoi(token); err == nil {
			return n
		}
	}
	return 0
}

func findLinkOrText(root *goquery.Selection, selector string) string {
	if selector == "" {
		return ""
	}
	sel, attr := splitSelector(selector)
	nodes := root.Find(sel)
	if sel == "" || sel == "self" {
		nodes = root
	}
	if nodes.Length() == 0 {
		return ""
	}
	if attr != "" {
		v, _ := nodes.First().Attr(attr)
		return strings.TrimSpace(v)
	}
	if href, ok := nodes.First().Attr("href"); ok {
		return strings.TrimSpace(href)
	}
	if nodes.First().Is("a") {
		if href, ok := nodes.First().Attr("href"); ok {
			return strings.TrimSpace(href)
		}
	}
	return collapse(nodes.First().Text())
}

func findText(root *goquery.Selection, selector string) string {
	if selector == "" {
		return ""
	}
	sel, attr := splitSelector(selector)
	nodes := root.Find(sel)
	if sel == "" || sel == "self" {
		nodes = root
	}
	if nodes.Length() == 0 {
		return ""
	}
	if attr != "" {
		v, _ := nodes.First().Attr(attr)
		return strings.TrimSpace(v)
	}
	return collapse(nodes.First().Text())
}

func splitSelector(selector string) (string, string) {
	if i := strings.LastIndex(selector, "@"); i >= 0 {
		attr := strings.TrimSpace(selector[i+1:])
		sel := strings.TrimSpace(selector[:i])
		if sel == "" {
			sel = "self"
		}
		return sel, attr
	}
	return strings.TrimSpace(selector), ""
}

func collapse(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}
