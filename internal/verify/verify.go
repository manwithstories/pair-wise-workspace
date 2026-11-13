package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"example.com/gkpack/internal/manifest"
	"example.com/gkpack/internal/pack"
)

// Issue is one failed resource and the reason it failed.
type Issue struct {
	Path   string `json:"path"`
	Block  string `json:"block,omitempty"`
	Reason string `json:"reason"`
}

// Result is a complete inspection report. Paths always contains every failed
// resource path, deduplicated and sorted.
type Result struct {
	Root          string   `json:"root"`
	CheckedFiles  int      `json:"checked_files"`
	CheckedBlocks int      `json:"checked_blocks"`
	FailedBlocks  int      `json:"failed_blocks"`
	FailedFiles   int      `json:"failed_files"`
	Paths         []string `json:"paths"`
	Issues        []Issue  `json:"issues"`
}

// Archive validates every block in an archive and maps broken blocks to every
// affected manifest path. It continues after individual block failures.
func Archive(path string) (Result, error) {
	reader, err := pack.Open(path)
	if err != nil {
		return Result{}, err
	}
	defer reader.Close()
	m := reader.Manifest()
	res := Result{Root: m.Root, CheckedFiles: len(m.Files), CheckedBlocks: len(reader.Chunks())}
	bad := make(map[string]string)
	for _, rec := range reader.Chunks() {
		if _, err := reader.ReadChunk(rec.Hash); err != nil {
			bad[rec.Hash] = err.Error()
		}
	}
	required := m.BlockSet()
	for hash := range required {
		if _, ok := reader.Chunk(hash); !ok {
			bad[hash] = "missing from archive chunk index"
		}
	}
	seenPaths := make(map[string]bool)
	seenIssues := make(map[string]bool)
	for _, f := range m.Files {
		for _, b := range f.Blocks {
			reason, ok := bad[b.Hash]
			if !ok {
				continue
			}
			if !seenPaths[f.Path] {
				seenPaths[f.Path] = true
				res.Paths = append(res.Paths, f.Path)
			}
			key := f.Path + "\x00" + b.Hash
			if !seenIssues[key] {
				seenIssues[key] = true
				res.Issues = append(res.Issues, Issue{Path: f.Path, Block: b.Hash, Reason: reason})
			}
		}
	}
	res.FailedBlocks = len(bad)
	res.FailedFiles = len(res.Paths)
	sort.Strings(res.Paths)
	sortIssues(res.Issues)
	return res, nil
}

// Tree validates an extracted resource tree against the archive manifest.
func Tree(archivePath, root string) (Result, error) {
	reader, err := pack.Open(archivePath)
	if err != nil {
		return Result{}, err
	}
	m := reader.Manifest()
	_ = reader.Close()
	res := Result{Root: m.Root, CheckedFiles: len(m.Files), CheckedBlocks: m.BlockCount()}
	seenIssues := make(map[string]bool)
	for _, f := range m.Files {
		rel, err := pack.CleanRelativePath(f.Path)
		if err != nil {
			res.Paths = append(res.Paths, f.Path)
			res.Issues = append(res.Issues, Issue{Path: f.Path, Reason: err.Error()})
			continue
		}
		full := filepath.Join(root, rel)
		info, err := os.Lstat(full)
		if errors.Is(err, fs.ErrNotExist) {
			res.Paths = append(res.Paths, f.Path)
			addIssue(&res, seenIssues, Issue{Path: f.Path, Reason: "missing file"})
			continue
		}
		if err != nil {
			res.Paths = append(res.Paths, f.Path)
			addIssue(&res, seenIssues, Issue{Path: f.Path, Reason: err.Error()})
			continue
		}
		if !info.Mode().IsRegular() {
			res.Paths = append(res.Paths, f.Path)
			addIssue(&res, seenIssues, Issue{Path: f.Path, Reason: "not a regular file"})
			continue
		}
		if uint64(info.Size()) != f.Size {
			res.Paths = append(res.Paths, f.Path)
			addIssue(&res, seenIssues, Issue{Path: f.Path, Reason: fmt.Sprintf("size %d, want %d", info.Size(), f.Size)})
			continue
		}
		got, err := fileSHA(full)
		if err != nil {
			res.Paths = append(res.Paths, f.Path)
			addIssue(&res, seenIssues, Issue{Path: f.Path, Reason: err.Error()})
			continue
		}
		if got != f.SHA256 {
			res.Paths = append(res.Paths, f.Path)
			addIssue(&res, seenIssues, Issue{Path: f.Path, Block: firstBlock(f), Reason: "sha256 mismatch"})
		}
	}
	sort.Strings(res.Paths)
	res.FailedFiles = len(res.Paths)
	// A tree check operates at file granularity; report the number of
	// file-level failures as failed blocks while still exposing exact
	// affected block identifiers in issues when available.
	res.FailedBlocks = res.FailedFiles
	sortIssues(res.Issues)
	return res, nil
}

// Both runs archive and tree checks and returns a combined report.
func Both(archivePath, root string) (Result, error) {
	a, err := Archive(archivePath)
	if err != nil {
		return Result{}, err
	}
	t, err := Tree(archivePath, root)
	if err != nil {
		return Result{}, err
	}
	return Merge(a, t), nil
}

// Merge combines two reports.
func Merge(a, b Result) Result {
	combined := a
	combined.Root = a.Root
	if b.CheckedBlocks > combined.CheckedBlocks {
		combined.CheckedBlocks = b.CheckedBlocks
	}
	combined.FailedBlocks += b.FailedBlocks
	seenPaths := make(map[string]bool)
	for _, p := range combined.Paths {
		seenPaths[p] = true
	}
	for _, p := range b.Paths {
		if !seenPaths[p] {
			seenPaths[p] = true
			combined.Paths = append(combined.Paths, p)
		}
	}
	combined.FailedFiles = len(combined.Paths)
	combined.Issues = append(combined.Issues, b.Issues...)
	sort.Strings(combined.Paths)
	sortIssues(combined.Issues)
	return combined
}

func addIssue(res *Result, seen map[string]bool, issue Issue) {
	key := issue.Path + "\x00" + issue.Block + "\x00" + issue.Reason
	if seen[key] {
		return
	}
	seen[key] = true
	res.Issues = append(res.Issues, issue)
}

func firstBlock(f manifest.File) string {
	if len(f.Blocks) > 0 {
		return f.Blocks[0].Hash
	}
	return ""
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sortIssues(issues []Issue) {
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Path != issues[j].Path {
			return issues[i].Path < issues[j].Path
		}
		if issues[i].Block != issues[j].Block {
			return issues[i].Block < issues[j].Block
		}
		return issues[i].Reason < issues[j].Reason
	})
}

// FormatSummary returns the terminal summary line used by the CLI and demo.
func (r Result) FormatSummary() string {
	if r.FailedBlocks == 0 && r.FailedFiles == 0 {
		return fmt.Sprintf("校验通过 root=%s 文件=%d 块=%d", prefix(r.Root), r.CheckedFiles, r.CheckedBlocks)
	}
	return fmt.Sprintf("校验失败块=%d 失败文件=%s", r.FailedBlocks, strings.Join(r.Paths, ","))
}

func prefix(root string) string {
	if len(root) < 8 {
		return root
	}
	return root[:8]
}
