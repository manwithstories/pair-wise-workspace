package garc_test

import (
	"errors"
	"testing"

	"hdrcheck/internal/fixture"
	"hdrcheck/internal/garc"
)

func buildValidBytes(t *testing.T) []byte {
	t.Helper()
	b, err := fixture.Scenarios()[0].Build() // hdr-valid.garc
	if err != nil {
		t.Fatalf("scenario build: %v", err)
	}
	data, err := b.Build()
	if err != nil {
		t.Fatalf("builder Build: %v", err)
	}
	return data
}

func TestParseValid(t *testing.T) {
	f, err := garc.Parse(buildValidBytes(t))
	if err != nil {
		t.Fatalf("Parse valid: %v", err)
	}
	if f.Header.Version != garc.SupportedVersion {
		t.Errorf("version=%d", f.Header.Version)
	}
	if len(f.VideoTracks()) != 3 {
		t.Errorf("video tracks=%d, want 3", len(f.VideoTracks()))
	}
	if !f.HasContainerHDR() {
		t.Error("container HDR flag not set")
	}
	if f.Header.Metadata.Length == 0 || f.Header.SEI.Length == 0 {
		t.Error("metadata/SEI regions empty")
	}
}

func TestParseBigEndian(t *testing.T) {
	// inherited 场景使用大端。
	var be []byte
	for _, sc := range fixture.Scenarios() {
		if sc.Name == "hdr-inherited.garc" {
			b, _ := sc.Build()
			be, _ = b.Build()
		}
	}
	f, err := garc.Parse(be)
	if err != nil {
		t.Fatalf("Parse big-endian: %v", err)
	}
	if f.Header.Endianness != garc.EndianBig {
		t.Errorf("endianness=%d", f.Header.Endianness)
	}
}

func TestParseRejectsCorruption(t *testing.T) {
	cases := map[string]string{
		"hdr-unknown-version.garc": "UNKNOWN_VERSION",
		"hdr-length-mismatch.garc": "LENGTH_MISMATCH",
		"hdr-truncated.garc":       "",
	}
	for _, sc := range fixture.Scenarios() {
		wantCode, known := cases[sc.Name]
		if !known {
			continue
		}
		b, _ := sc.Build()
		data, _ := b.Build()
		_, err := garc.Parse(data)
		if err == nil {
			t.Errorf("%s: expected parse error, got nil", sc.Name)
			continue
		}
		var ge *garc.Error
		if !errors.As(err, &ge) {
			t.Errorf("%s: error is not *garc.Error: %v", sc.Name, err)
			continue
		}
		if wantCode != "" && ge.Code != wantCode {
			t.Errorf("%s: code=%s, want %s", sc.Name, ge.Code, wantCode)
		}
	}
}

func TestParseBadMagicAndTruncatedHeader(t *testing.T) {
	if _, err := garc.Parse([]byte("XXXX" + "000000000000")); err == nil {
		t.Error("bad magic accepted")
	}
	if _, err := garc.Parse([]byte("GARC")); err == nil {
		t.Error("4-byte file accepted as header")
	}
}
