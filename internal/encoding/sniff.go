// Package encoding detects byte encodings and decodes to UTF-8 text.
package encoding

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// Result describes a decoded input.
type Result struct {
	Name       string  // detected encoding label
	Confidence float64 // 0..1
	Text       string  // decoded UTF-8 text
	BadLines   []int   // 1-based lines with undecodable bytes (isolated)
}

// Sniff detects BOM, validates UTF-8, then falls back to UTF-16 / GB18030.
func Sniff(data []byte) Result {
	if r, ok := sniffBOM(data); ok {
		return r
	}
	if utf8.Valid(data) {
		return Result{Name: "UTF-8", Confidence: 0.99, Text: string(data)}
	}
	if r, ok := sniffUTF16NoBOM(data); ok {
		return r
	}
	return decodeGB18030(data)
}

func sniffBOM(data []byte) (Result, bool) {
	switch {
	case bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}):
		body := data[3:]
		if !utf8.Valid(body) {
			r := decodeGB18030(body)
			r.Name = "UTF-8-BOM+GB18030"
			r.Confidence = 0.55
			return r, true
		}
		return Result{Name: "UTF-8-BOM", Confidence: 1.0, Text: string(body)}, true
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}):
		return decodeUTF16(data[2:], unicode.LittleEndian, "UTF-16LE"), true
	case bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		return decodeUTF16(data[2:], unicode.BigEndian, "UTF-16BE"), true
	}
	return Result{}, false
}

// sniffUTF16NoBOM uses NUL-byte distribution to guess UTF-16 without a BOM.
func sniffUTF16NoBOM(data []byte) (Result, bool) {
	if len(data) < 8 {
		return Result{}, false
	}
	var evenNUL, oddNUL int
	n := len(data) - len(data)%2
	for i := 0; i < n; i += 2 {
		if data[i] == 0 {
			evenNUL++
		}
		if data[i+1] == 0 {
			oddNUL++
		}
	}
	pairs := n / 2
	if oddNUL > pairs/2 && evenNUL == 0 {
		return decodeUTF16(data, unicode.LittleEndian, "UTF-16LE"), true
	}
	if evenNUL > pairs/2 && oddNUL == 0 {
		return decodeUTF16(data, unicode.BigEndian, "UTF-16BE"), true
	}
	return Result{}, false
}

func decodeUTF16(data []byte, endian unicode.Endianness, name string) Result {
	dec := unicode.UTF16(endian, unicode.IgnoreBOM).NewDecoder()
	out, err := transformBytes(dec, data)
	if err != nil {
		return Result{Name: name, Confidence: 0.4, Text: string(out), BadLines: badLines(string(out))}
	}
	return Result{Name: name, Confidence: 0.95, Text: string(out)}
}

func decodeGB18030(data []byte) Result {
	dec := simplifiedchinese.GB18030.NewDecoder()
	out, err := transformBytes(dec, data)
	if err != nil {
		text := string(out)
		return Result{Name: "GB18030", Confidence: 0.6, Text: text, BadLines: badLines(text)}
	}
	return Result{Name: "GB18030", Confidence: 0.9, Text: string(out)}
}

func transformBytes(dec transform.Transformer, data []byte) ([]byte, error) {
	out := make([]byte, 0, len(data))
	buf := make([]byte, 4096)
	src := data
	for len(src) > 0 {
		ndst, nsrc, err := dec.Transform(buf, src, false)
		out = append(out, buf[:ndst]...)
		src = src[nsrc:]
		switch err {
		case nil, transform.ErrShortDst:
			// Progress made or output buffer full: keep going.
		case transform.ErrShortSrc:
			// Incomplete sequence at end of input.
			out = append(out, []byte(string(utf8.RuneError))...)
			src = nil
		default:
			// Undecodable bytes: emit replacement char, skip one byte.
			out = append(out, []byte(string(utf8.RuneError))...)
			if nsrc == 0 {
				src = src[1:]
			}
			dec.Reset()
		}
	}
	return out, nil
}

// badLines returns 1-based line numbers containing U+FFFD replacements.
func badLines(text string) []int {
	var lines []int
	for i, line := range strings.Split(text, "\n") {
		if strings.ContainsRune(line, utf8.RuneError) {
			lines = append(lines, i+1)
		}
	}
	return lines
}

// NormalizeNewlines converts CRLF and lone CR to LF.
func NormalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}
