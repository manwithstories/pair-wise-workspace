package pipe

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"archivecheck/internal/fault"
)

// EntryKind distinguishes initialization segments from media fragments.
type EntryKind string

const (
	KindInit  EntryKind = "init"
	KindMedia EntryKind = "media"
)

// Entry is one explicitly listed input file.
type Entry struct {
	Kind   EntryKind
	Name   string
	Path   string
	SHA256 string
	Bytes  int64
}

// Group pairs one init segment with the media fragments that follow it.
type Group struct {
	Init  *Entry
	Media []*Entry
}

// Manifest is an ordered, validated input list rooted at a local directory.
type Manifest struct {
	Root    string
	Path    string
	Groups  []*Group
	Digest  string
	Entries []*Entry
}

// LoadManifest parses an explicit text manifest. Each non-empty, non-comment
// line is "<init|media> <relative-name>". Paths must stay inside root, and
// every target must be a regular file (symlinks are rejected).
func LoadManifest(root, manifestPath string) (*Manifest, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, fault.Wrap(err, fault.KindPath, manifestPath, "", 0, "cannot resolve root")
	}
	f, err := os.Open(manifestPath)
	if err != nil {
		return nil, fault.Wrap(err, fault.KindPath, manifestPath, "", 0, "cannot open manifest")
	}
	defer f.Close()

	m := &Manifest{Root: rootAbs, Path: manifestPath}
	var current *Group
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fault.New(fault.KindPath, manifestPath, "", int64(lineNo),
				fmt.Sprintf("manifest line %d must be \"<init|media> <name>\"", lineNo))
		}
		kind := EntryKind(fields[0])
		if kind != KindInit && kind != KindMedia {
			return nil, fault.New(fault.KindPath, manifestPath, "", int64(lineNo),
				fmt.Sprintf("manifest line %d has unknown kind %q", lineNo, fields[0]))
		}
		entry, err := resolveEntry(rootAbs, fields[1], kind)
		if err != nil {
			return nil, err
		}
		m.Entries = append(m.Entries, entry)
		if kind == KindInit {
			current = &Group{Init: entry}
			m.Groups = append(m.Groups, current)
		} else {
			if current == nil {
				return nil, fault.New(fault.KindPath, manifestPath, entry.Name, int64(lineNo),
					"media fragment listed before any init segment")
			}
			current.Media = append(current.Media, entry)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fault.Wrap(err, fault.KindPath, manifestPath, "", 0, "cannot read manifest")
	}
	if len(m.Groups) == 0 {
		return nil, fault.New(fault.KindPath, manifestPath, "", 0, "manifest lists no init segment")
	}
	sum, err := manifestDigest(m.Entries)
	if err != nil {
		return nil, err
	}
	m.Digest = sum
	return m, nil
}

func resolveEntry(rootAbs, name string, kind EntryKind) (*Entry, error) {
	if filepath.IsAbs(name) {
		return nil, fault.New(fault.KindPath, name, "", 0, "absolute paths are rejected")
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fault.New(fault.KindPath, name, "", 0, "path escapes the archive root")
	}
	path := filepath.Join(rootAbs, clean)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fault.Wrap(err, fault.KindNotRegular, clean, "", 0, "input entry missing")
	}
	if !info.Mode().IsRegular() {
		return nil, fault.New(fault.KindNotRegular, clean, "", 0,
			fmt.Sprintf("input is %s, not a regular file", info.Mode().Type()))
	}
	digest, err := FileSHA256(path)
	if err != nil {
		return nil, err
	}
	return &Entry{Kind: kind, Name: clean, Path: path, SHA256: digest, Bytes: info.Size()}, nil
}

// FileSHA256 hashes a file for the manifest digest.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fault.Wrap(err, fault.KindNotRegular, path, "", 0, "cannot open input")
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fault.Wrap(err, fault.KindShortRead, path, "", 0, "cannot hash input")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// manifestDigest covers kind, name, and content digest in manifest order, so
// any edit, reorder or byte change invalidates resumption.
func manifestDigest(entries []*Entry) (string, error) {
	h := sha256.New()
	for _, e := range entries {
		if _, err := fmt.Fprintf(h, "%s %s %s\n", e.Kind, e.Name, e.SHA256); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
