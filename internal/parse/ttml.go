package parse

import (
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"subtool/internal/model"
)

var (
	ttTagRE     = regexp.MustCompile(`<tt\b[^>]*>`)
	attrRE      = regexp.MustCompile(`([:\w-]+)\s*=\s*"([^"]*)"`)
	pTagRE      = regexp.MustCompile(`(?s)<p\b([^>]*)>(.*?)</p>`)
	stripTagsRE = regexp.MustCompile(`<[^>]+>`)
	clockRE     = regexp.MustCompile(`^(\d{2,3}):([0-5]\d):([0-5]\d)(?:\.(\d+))?(?::(\d+)(?:\.(\d+))?)?$`)
	offsetRE    = regexp.MustCompile(`^(\d+(?:\.\d+)?)(h|m|s|ms|f|t)$`)
)

// ParseTTML parses a TTML/DFXP document with a hand-rolled lexer.
// Clock, offset, frame ("24f") and tick ("100t") time expressions all
// converge to rational milliseconds.
func ParseTTML(text string) *Result {
	text = strings.Join(NormalizeLines(text), "\n")
	res := &Result{Header: map[string]string{}}
	tickRate := int64(1)
	frameRate := int64(30)
	if m := ttTagRE.FindString(text); m != "" {
		res.Header["tt"] = m
		for _, a := range attrRE.FindAllStringSubmatch(m, -1) {
			switch strings.ToLower(a[1]) {
			case "ttp:tickrate":
				if v, err := strconv.ParseInt(a[2], 10, 64); err == nil && v > 0 {
					tickRate = v
				}
			case "ttp:framerate":
				if v, err := strconv.ParseInt(a[2], 10, 64); err == nil && v > 0 {
					frameRate = v
				}
			}
		}
	}
	lineOf := func(pos int) int { return 1 + strings.Count(text[:pos], "\n") }
	seq := 0
	for _, loc := range pTagRE.FindAllStringSubmatchIndex(text, -1) {
		attrs := text[loc[2]:loc[3]]
		body := text[loc[4]:loc[5]]
		lineNo := lineOf(loc[0])
		var id, begin, end, dur string
		for _, a := range attrRE.FindAllStringSubmatch(attrs, -1) {
			switch strings.ToLower(a[1]) {
			case "xml:id", "id":
				id = a[2]
			case "begin":
				begin = a[2]
			case "end":
				end = a[2]
			case "dur":
				dur = a[2]
			}
		}
		sRat, err := ParseTTMLTime(begin, tickRate, frameRate)
		if err != nil {
			res.Issues = append(res.Issues, model.Issue{
				Line: lineNo, Code: model.CodeBadTimecode, Msg: err.Error(),
			})
			continue
		}
		var eRat *big.Rat
		if end != "" {
			eRat, err = ParseTTMLTime(end, tickRate, frameRate)
		} else if dur != "" {
			var d *big.Rat
			d, err = ParseTTMLTime(dur, tickRate, frameRate)
			if err == nil {
				eRat = new(big.Rat).Add(sRat, d)
			}
		} else {
			err = fmt.Errorf("missing end/dur")
		}
		if err != nil {
			res.Issues = append(res.Issues, model.Issue{
				Line: lineNo, Code: model.CodeBadTimecode, Msg: err.Error(),
			})
			continue
		}
		seq++
		if id == "" {
			id = fmt.Sprintf("%04d", seq)
		}
		plain := strings.TrimSpace(unescapeXML(stripTagsRE.ReplaceAllString(body, "")))
		tags := tagRE.FindAllString(body, -1)
		c := model.Cue{
			ID: id, Start: sRat, End: eRat, Text: plain,
			Tags: tags, SourceLine: lineNo,
		}
		if issue := ValidateCue(&c); issue != nil {
			res.Issues = append(res.Issues, *issue)
			continue
		}
		res.Cues = append(res.Cues, c)
	}
	return res
}

// ParseTTMLTime converts one TTML time expression to rational ms.
func ParseTTMLTime(s string, tickRate, frameRate int64) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty time expression")
	}
	if m := clockRE.FindStringSubmatch(s); m != nil {
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		sec, _ := strconv.Atoi(m[3])
		ms := new(big.Rat).SetInt64(int64(((h*60 + mi) * 60) + sec))
		ms.Mul(ms, big.NewRat(1000, 1))
		if m[4] != "" {
			den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(m[4]))), nil)
			num, _ := new(big.Int).SetString(m[4], 10)
			frac := new(big.Rat).SetFrac(num, den)
			ms.Add(ms, frac.Mul(frac, big.NewRat(1000, 1)))
		}
		if m[5] != "" {
			frames, _ := strconv.ParseInt(m[5], 10, 64)
			f := big.NewRat(frames*1000, frameRate)
			ms.Add(ms, f)
		}
		return ms, nil
	}
	if m := offsetRE.FindStringSubmatch(s); m != nil {
		val, ok := new(big.Rat).SetString(m[1])
		if !ok {
			return nil, fmt.Errorf("bad offset value %q", s)
		}
		switch m[2] {
		case "h":
			return val.Mul(val, big.NewRat(3600000, 1)), nil
		case "m":
			return val.Mul(val, big.NewRat(60000, 1)), nil
		case "s":
			return val.Mul(val, big.NewRat(1000, 1)), nil
		case "ms":
			return val, nil
		case "f":
			return val.Mul(val, big.NewRat(1000, frameRate)), nil
		case "t":
			return val.Mul(val, big.NewRat(1000, tickRate)), nil
		}
	}
	return nil, fmt.Errorf("unsupported time expression %q", s)
}

var xmlUnescaper = strings.NewReplacer(
	"&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'", "&amp;", "&",
)

func unescapeXML(s string) string { return xmlUnescaper.Replace(s) }
