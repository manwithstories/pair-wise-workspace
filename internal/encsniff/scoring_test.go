package encsniff

import (
	"testing"

	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/transform"
)

func TestDetectKnownEncodings(t *testing.T) {
	tests := []struct {
		name     string
		expected string
		body     string
		enc      transform.Transformer
	}{
		{"gb18030", "GB18030", "1\n00:00:00,000 --> 00:00:01,000\n这件事必须在天亮前完成。\n", simplifiedchinese.GB18030.NewEncoder()},
		{"big5", "Big5", "WEBVTT\n\n1\n00:00:00.000 --> 00:00:01.000\n這個故事從這裡開始。\n", traditionalchinese.Big5.NewEncoder()},
		{"shift-jis", "Shift-JIS", "[Events]\n1\n00:00:00,000 --> 00:00:01,000\nこれは物語の始まりです。\n", japanese.ShiftJIS.NewEncoder()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, _, err := transform.Bytes(test.enc, []byte(test.body))
			if err != nil {
				t.Fatal(err)
			}
			result, err := Detect(data)
			if err != nil {
				t.Fatal(err)
			}
			if result.Name != test.expected {
				t.Fatalf("encoding=%s want %s (confidence %.3f)", result.Name, test.expected, result.Confidence)
			}
			if result.Confidence < LowConfidence || result.Low {
				t.Fatalf("unexpected low confidence: %.3f", result.Confidence)
			}
			if string(result.Text) != test.body {
				t.Fatalf("decoded text mismatch: %q", result.Text)
			}
		})
	}
}

func TestDetectUTF8BOMAndLowConfidence(t *testing.T) {
	bom := append([]byte{0xef, 0xbb, 0xbf}, []byte("hello")...)
	result, err := Detect(bom)
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != "UTF-8-BOM" || result.Confidence != 1 || string(result.Text) != "hello" {
		t.Fatalf("BOM was not prioritized: %#v", result)
	}
	low, err := Detect([]byte("\xff\x80\x81 default"))
	if err != nil {
		t.Fatal(err)
	}
	if !low.Low || low.Confidence >= LowConfidence {
		t.Fatalf("expected low confidence, got %#v", low)
	}
}
