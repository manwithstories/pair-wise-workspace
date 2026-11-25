package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"mediaproc/internal/pipeline"
	"mediaproc/internal/verify"
)

type manifestEntry struct {
	name string
	hash string
}

type publisher struct {
	dir     string
	mu      sync.Mutex
	records []verify.Record
	files   []manifestEntry
}

func newPublisher(out string) (*publisher, error) {
	segDir := filepath.Join(out, "segments")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return nil, err
	}
	return &publisher{dir: out}, nil
}

func (p *publisher) Publish(_ context.Context, item pipeline.Item) error {
	s := item.Segment
	rec := verify.RecordFromSegment(s)
	rec.MP4Validated = s.MP4Validated

	jsonData, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	jsonData = append(jsonData, '\n')

	base := fmt.Sprintf("segment-%03d", s.Index)
	jsonName := filepath.ToSlash(filepath.Join("segments", base+".json"))
	if err := os.WriteFile(filepath.Join(p.dir, jsonName), jsonData, 0o644); err != nil {
		return err
	}
	p.mu.Lock()
	p.records = append(p.records, rec)
	jsonSum := sha256.Sum256(jsonData)
	p.files = append(p.files, manifestEntry{name: jsonName, hash: hex.EncodeToString(jsonSum[:])})
	p.mu.Unlock()

	if !s.MP4Validated {
		return nil
	}
	mp4Data, err := verify.BuildMP4Sidecar(uint32(s.Index), s.StartNS, []byte(s.SHA256))
	if err != nil {
		return err
	}
	if err := verify.ValidateMP4Sidecar(mp4Data); err != nil {
		return err
	}
	mp4Name := filepath.ToSlash(filepath.Join("segments", base+".mp4"))
	if err := os.WriteFile(filepath.Join(p.dir, mp4Name), mp4Data, 0o644); err != nil {
		return err
	}
	mp4Sum := sha256.Sum256(mp4Data)
	p.mu.Lock()
	p.files = append(p.files, manifestEntry{name: mp4Name, hash: hex.EncodeToString(mp4Sum[:])})
	p.mu.Unlock()
	return nil
}

func (p *publisher) Records() []verify.Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]verify.Record(nil), p.records...)
}

func (p *publisher) WriteManifest() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var content string
	for _, f := range p.files {
		content += fmt.Sprintf("%s  %s\n", f.hash, f.name)
	}
	path := filepath.Join(p.dir, "manifest.sha256")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// CheckManifest verifies every hash written to manifest.sha256.
func CheckManifest(out string) (int, int, error) {
	data, err := os.ReadFile(filepath.Join(out, "manifest.sha256"))
	if err != nil {
		return 0, 0, err
	}
	lines := splitLines(string(data))
	total, ok := 0, 0
	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := splitFields(line)
		if len(fields) != 2 {
			continue
		}
		total++
		raw, err := os.ReadFile(filepath.Join(out, fields[1]))
		if err != nil {
			return total, ok, err
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) == fields[0] {
			ok++
		}
	}
	return ok, total, nil
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i, r := range s {
		if r == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func splitFields(s string) []string {
	var fields []string
	start := -1
	for i, r := range s {
		if r == ' ' {
			if start >= 0 {
				fields = append(fields, s[start:i])
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		fields = append(fields, s[start:])
	}
	return fields
}
