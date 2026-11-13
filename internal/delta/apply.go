package delta

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

const (
	checkpointName = ".gkpack-apply.log"
	tempPrefix     = ".gkpack-tmp-"
)

// ApplyOptions controls reconstruction.
type ApplyOptions struct {
	BaseArchive    string
	PatchPath      string
	OutputDir      string
	Reverse        bool
	ExpectVersion  string
	SkipRootRescan bool
}

// ApplyResult describes a completed reconstruction.
type ApplyResult struct {
	Root          string
	BaseVersion   string
	TargetVersion string
	Files         int
	Restored      int
	Removed       int
	ResumedFiles  int
	WrittenFiles  int
	ReusedBlocks  int
	NewBlocks     int
}

type checkpointRecord struct {
	Root string `json:"root"`
	Path string `json:"path"`
}

// Apply rebuilds the target version from a baseline archive plus patch. Each
// file is written to a sibling temporary file and renamed only after its size
// and SHA-256 match the target manifest.
func Apply(opts ApplyOptions) (ApplyResult, error) {
	if opts.BaseArchive == "" || opts.PatchPath == "" || opts.OutputDir == "" {
		return ApplyResult{}, errors.New("base archive, patch, and output directory are required")
	}
	base, err := pack.Open(opts.BaseArchive)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("open baseline archive: %w", err)
	}
	defer base.Close()
	patch, err := Open(opts.PatchPath)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("open patch: %w", err)
	}
	defer patch.Close()

	header := patch.Header()
	expectedBase := header.BaseRoot
	if opts.Reverse {
		expectedBase = header.TargetRoot
	}
	if base.Root() != expectedBase {
		return ApplyResult{}, fmt.Errorf("baseline root mismatch: archive=%s expected=%s", base.Root(), expectedBase)
	}
	target := patch.ManifestFor(opts.Reverse)
	baseManifest := patch.ManifestFor(!opts.Reverse)
	if target.Root != header.TargetRoot && !opts.Reverse {
		return ApplyResult{}, fmt.Errorf("target manifest root mismatch: %s", target.Root)
	}
	if target.Root != header.BaseRoot && opts.Reverse {
		return ApplyResult{}, fmt.Errorf("reverse target manifest root mismatch: %s", target.Root)
	}
	targetVersion := header.TargetVersion
	baseVersion := header.BaseVersion
	if opts.Reverse {
		targetVersion, baseVersion = header.BaseVersion, header.TargetVersion
	}
	if opts.ExpectVersion != "" && opts.ExpectVersion != targetVersion {
		return ApplyResult{}, fmt.Errorf("target version mismatch: patch targets %q, requested %q", targetVersion, opts.ExpectVersion)
	}

	patchSet := make(map[string]bool)
	if opts.Reverse {
		for _, rec := range header.Reverse.Chunks {
			patchSet[rec.Hash] = true
		}
	} else {
		for _, rec := range header.Forward.Chunks {
			patchSet[rec.Hash] = true
		}
	}
	reused, added, err := validateAvailability(base, target, patchSet)
	if err != nil {
		return ApplyResult{}, err
	}

	if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
		return ApplyResult{}, err
	}
	if info, err := os.Stat(opts.OutputDir); err != nil || !info.IsDir() {
		return ApplyResult{}, fmt.Errorf("output %q is not a directory", opts.OutputDir)
	}
	checkpointPath := filepath.Join(opts.OutputDir, checkpointName)
	resumedLog := loadCheckpoint(checkpointPath, target.Root)
	if err := cleanupTemps(opts.OutputDir); err != nil {
		return ApplyResult{}, err
	}
	checkpoint, err := os.OpenFile(checkpointPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return ApplyResult{}, err
	}
	defer checkpoint.Close()
	if len(resumedLog) == 0 {
		// A stale log belongs to another target. Start with an empty log while
		// preserving already-complete files, which are revalidated below.
		if err := os.Truncate(checkpointPath, 0); err != nil {
			return ApplyResult{}, err
		}
	}

	restored, removed := fileCounts(baseManifest, target)
	result := ApplyResult{
		Root:          target.Root,
		BaseVersion:   baseVersion,
		TargetVersion: targetVersion,
		Files:         len(target.Files),
		Restored:      restored,
		Removed:       removed,
		ReusedBlocks:  reused,
		NewBlocks:     added,
	}

	for _, f := range target.Files {
		skip, err := writeFile(opts.OutputDir, f, base, patch, opts.Reverse)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("%s: %w", f.Path, err)
		}
		if skip {
			result.ResumedFiles++
			continue
		}
		result.WrittenFiles++
		if err := appendCheckpoint(checkpoint, checkpointRecord{Root: target.Root, Path: f.Path}); err != nil {
			return ApplyResult{}, err
		}
	}

	if err := removeBaseOnlyFiles(opts.OutputDir, baseManifest, target); err != nil {
		return ApplyResult{}, err
	}
	if err := cleanupTemps(opts.OutputDir); err != nil {
		return ApplyResult{}, err
	}
	if err := checkpoint.Close(); err != nil {
		return ApplyResult{}, err
	}
	if err := os.Remove(checkpointPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ApplyResult{}, err
	}
	if !opts.SkipRootRescan {
		actual, err := pack.ScanTree(opts.OutputDir)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("recompute output root: %w", err)
		}
		if actual.Root != target.Root {
			return ApplyResult{}, fmt.Errorf("output root mismatch: got %s, want %s", actual.Root, target.Root)
		}
	}
	return result, nil
}

func validateAvailability(base *pack.Reader, target manifest.Manifest, patchSet map[string]bool) (reused, added int, err error) {
	for _, f := range target.Files {
		for _, b := range f.Blocks {
			if patchSet[b.Hash] {
				added++
				continue
			}
			if _, ok := base.Chunk(b.Hash); !ok {
				return 0, 0, fmt.Errorf("%s: block %s is missing from patch and baseline", f.Path, b.Hash)
			}
			reused++
		}
	}
	return reused, added, nil
}

func fileCounts(base, target manifest.Manifest) (restored, removed int) {
	basePaths := make(map[string]bool, len(base.Files))
	for _, f := range base.Files {
		basePaths[f.Path] = true
	}
	targetPaths := make(map[string]bool, len(target.Files))
	for _, f := range target.Files {
		targetPaths[f.Path] = true
	}
	for p := range targetPaths {
		if !basePaths[p] {
			restored++
		}
	}
	for p := range basePaths {
		if !targetPaths[p] {
			removed++
		}
	}
	return restored, removed
}

func writeFile(outDir string, f manifest.File, base *pack.Reader, patch *Reader, reverse bool) (bool, error) {
	rel, err := pack.CleanRelativePath(f.Path)
	if err != nil {
		return false, err
	}
	full := filepath.Join(outDir, rel)
	if ok, err := matchesFile(full, f); err != nil {
		return false, err
	} else if ok {
		return true, nil
	}
	if err := ensureSafeParents(outDir, filepath.Dir(rel)); err != nil {
		return false, err
	}
	parent := filepath.Dir(full)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(parent, tempPrefix+"*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	hasher := sha256.New()
	var written uint64
	for _, block := range f.Blocks {
		var raw []byte
		if patch.HasChunk(reverse, block.Hash) {
			raw, err = patch.ReadNewChunk(reverse, block.Hash)
		} else {
			raw, err = base.ReadChunk(block.Hash)
		}
		if err != nil {
			return false, err
		}
		if len(raw) != int(block.Size) {
			return false, fmt.Errorf("block %s size %d, want %d", block.Hash, len(raw), block.Size)
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != block.Hash {
			return false, fmt.Errorf("block %s failed per-block hash check", block.Hash)
		}
		if _, err := tmp.Write(raw); err != nil {
			return false, err
		}
		_, _ = hasher.Write(raw)
		written += uint64(len(raw))
	}
	if written != f.Size {
		return false, fmt.Errorf("file size %d, want %d", written, f.Size)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != f.SHA256 {
		return false, errors.New("file SHA-256 mismatch")
	}
	if err := tmp.Chmod(0o644); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmpName, full); err != nil {
		return false, err
	}
	committed = true
	return false, nil
}

func matchesFile(path string, f manifest.File) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || uint64(info.Size()) != f.Size {
		return false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == f.SHA256, nil
}

func ensureSafeParents(root, relDir string) error {
	if relDir == "." || relDir == "" {
		return nil
	}
	current := root
	parts := strings.Split(filepath.Clean(relDir), string(filepath.Separator))
	for _, part := range parts {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write through symlink %s", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", current)
		}
	}
	return nil
}

func loadCheckpoint(path, root string) map[string]bool {
	out := make(map[string]bool)
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var rec checkpointRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		if rec.Root == root && rec.Path != "" {
			out[rec.Path] = true
		}
	}
	return out
}

func appendCheckpoint(f *os.File, rec checkpointRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = f.Write(data)
	return err
}

func cleanupTemps(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), tempPrefix) {
			return os.Remove(path)
		}
		return nil
	})
}

func removeBaseOnlyFiles(outDir string, base, target manifest.Manifest) error {
	targetPaths := make(map[string]bool, len(target.Files))
	for _, f := range target.Files {
		targetPaths[f.Path] = true
	}
	var remove []string
	for _, f := range base.Files {
		if targetPaths[f.Path] {
			continue
		}
		remove = append(remove, f.Path)
	}
	sort.Strings(remove)
	for _, p := range remove {
		rel, err := pack.CleanRelativePath(p)
		if err != nil {
			return err
		}
		full := filepath.Join(outDir, rel)
		info, err := os.Lstat(full)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.IsDir() {
			continue
		}
		if err := os.Remove(full); err != nil {
			return err
		}
	}
	removeEmptyDirs(outDir, outDir)
	return nil
}

func removeEmptyDirs(root, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		child := filepath.Join(dir, entry.Name())
		removeEmptyDirs(root, child)
		remaining, err := os.ReadDir(child)
		if err == nil && len(remaining) == 0 {
			_ = os.Remove(child)
		}
	}
}
