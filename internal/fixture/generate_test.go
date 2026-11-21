package fixture

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateLayout(t *testing.T) {
	dir := t.TempDir()
	l, err := Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hls/media.m3u8")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "media/video/seg-06.m4s")); !os.IsNotExist(err) {
		t.Fatalf("missing segment should be absent: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "media/video/seg-01.m4s"))
	if err != nil || info.Size() <= 0 {
		t.Fatalf("video seg1: %v", err)
	}
	if _, err := GenerateRTP(l.RTPFile, 60, 17); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(l.RTPFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 100 {
		t.Fatalf("rtp capture too small: %d", len(data))
	}
	// First record length must match its framing.
	n := binary.BigEndian.Uint32(data[:4])
	if int(n) > len(data)-4 {
		t.Fatalf("bad rtp record length %d", n)
	}
}

func TestGenerateDeterministic(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if _, err := Generate(a); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(b); err != nil {
		t.Fatal(err)
	}
	ha, _, err := hashDir(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, _, err := hashDir(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatal("fixture generation is not deterministic")
	}
}

func hashDir(dir string) (string, int, error) {
	h := sha256.New()
	count := 0
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		h.Write([]byte(rel))
		h.Write(data)
		count++
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), count, err
}
