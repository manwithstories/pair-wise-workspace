package render

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"fieldcut/internal/media"
)

// ManifestName is the program's own ownership marker inside an output dir.
const ManifestName = "export.json"

// Ownership marks a directory as written by fieldcut. Any other content in
// the target directory is refused rather than overwritten.
type Ownership struct {
	Program string `json:"program"`
	Version int    `json:"version"`
	Frames  int    `json:"frames"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Root    string `json:"root_sha256"`
	Config  string `json:"config_sha256"`
	Source  string `json:"source_sha256"`
	Resumed bool   `json:"resumed"`
}

// Exporter writes frames under dir/frames in atomic per-frame and manifest
// steps. It is safe to interrupt: on open, every program-owned frame already
// on disk is re-hashed and adopted, so a resumed run produces the same root
// hash as a complete run. A changed configuration or source hash invalidates
// the cache by clearing only program-owned artifacts.
type Exporter struct {
	mu      sync.Mutex
	dir     string
	partial map[int64]string // committed frame hashes for resumability
}

// Open prepares an output directory. It refuses non-empty directories that
// hold content this program did not write, invalidates stale program-owned
// output when cfg or source changed, and adopts frames left by an interrupted
// run only after re-hashing them.
func Open(dir, cfg, source string) (*Exporter, *Ownership, error) {
	if err := os.MkdirAll(filepath.Join(dir, "frames"), 0o755); err != nil {
		return nil, nil, media.New(media.KindConflict, "export.open", "", -1, err)
	}
	ownPath := filepath.Join(dir, ManifestName)
	data, err := os.ReadFile(ownPath)
	var own Ownership
	switch {
	case err == nil:
		if json.Unmarshal(data, &own) != nil || own.Program != "fieldcut" {
			return nil, nil, media.Newf(media.KindConflict, "export.open", "", -1, "%s is not a fieldcut manifest", ownPath)
		}
		if own.Config != cfg || own.Source != source {
			if err := clearOwned(dir); err != nil {
				return nil, nil, err
			}
			own = Ownership{}
		}
	case errors.Is(err, os.ErrNotExist):
		if err := refuseForeign(dir); err != nil {
			return nil, nil, err
		}
	default:
		return nil, nil, media.New(media.KindConflict, "export.open", "", -1, err)
	}
	ex := &Exporter{dir: dir, partial: make(map[int64]string)}
	if err := ex.adopt(); err != nil {
		return nil, nil, err
	}
	return ex, &own, nil
}

// refuseForeign rejects entries in the output root or frame directory that
// fieldcut cannot prove it wrote: unknown root entries and malformed frame
// names are never overwritten.
func refuseForeign(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return media.New(media.KindConflict, "export.open", "", -1, err)
	}
	for _, e := range ents {
		if e.Name() == "frames" && e.IsDir() {
			continue
		}
		return media.Newf(media.KindConflict, "export.open", "", -1, "refusing to overwrite non-fieldcut content %q", e.Name())
	}
	frames, err := os.ReadDir(filepath.Join(dir, "frames"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return media.New(media.KindConflict, "export.open", "", -1, err)
	}
	for _, e := range frames {
		if _, ok := parseFrameName(e.Name()); !ok || e.IsDir() {
			return media.Newf(media.KindConflict, "export.open", "", -1, "refusing to overwrite non-fieldcut frame %q", e.Name())
		}
	}
	return nil
}

// clearOwned removes only files this program created: numbered PNG frames,
// their temp files and the ownership manifest.
func clearOwned(dir string) error {
	frames, err := os.ReadDir(filepath.Join(dir, "frames"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return media.New(media.KindConflict, "export.invalidate", "", -1, err)
	}
	for _, e := range frames {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if _, ok := parseFrameName(name); !ok && !strings.HasSuffix(name, ".png.tmp") && !strings.HasSuffix(name, ".png") {
			return media.Newf(media.KindConflict, "export.invalidate", "", -1, "refusing to delete foreign frame %q", name)
		}
		if err := os.Remove(filepath.Join(dir, "frames", name)); err != nil {
			return media.New(media.KindConflict, "export.invalidate", "", -1, err)
		}
	}
	if err := os.Remove(filepath.Join(dir, ManifestName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return media.New(media.KindConflict, "export.invalidate", "", -1, err)
	}
	return nil
}

// adopt re-hashes frames left by an interrupted run and records them, so the
// resumed pass skips regenerating them without trusting unverified bytes.
func (e *Exporter) adopt() error {
	frames, err := os.ReadDir(filepath.Join(e.dir, "frames"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return media.New(media.KindConflict, "export.adopt", "", -1, err)
	}
	for _, ent := range frames {
		if ent.IsDir() {
			continue
		}
		seq, ok := parseFrameName(ent.Name())
		if !ok {
			continue
		}
		path := filepath.Join(e.dir, "frames", ent.Name())
		sum, err := hashFile(path)
		if err != nil {
			return media.New(media.KindConflict, "export.adopt", "", seq, err)
		}
		e.partial[seq] = sum
	}
	return nil
}

// parseFrameName recognises the canonical 00000.png frame name.
func parseFrameName(name string) (int64, bool) {
	if len(name) != 9 || filepath.Ext(name) != ".png" {
		return 0, false
	}
	digits := name[:5]
	var seq int64
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
		seq = seq*10 + int64(c-'0')
	}
	return seq, true
}

// Commit writes one frame to a temp file, fsyncs it, renames it into place
// and records its hash. Commits are serialized so a resumed pass sees a
// consistent partial map.
func (e *Exporter) Commit(seq int64, img image.Image) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rel := frameFileName(seq)
	dst := filepath.Join(e.dir, rel)
	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", media.New(media.KindConflict, "export.commit", "", seq, err)
	}
	if err := Encode(f, img); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", media.New(media.KindCorrupt, "export.commit", "", seq, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", media.New(media.KindConflict, "export.commit", "", seq, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", media.New(media.KindConflict, "export.commit", "", seq, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", media.New(media.KindConflict, "export.commit", "", seq, err)
	}
	sum, err := hashFile(dst)
	if err != nil {
		return "", media.New(media.KindConflict, "export.commit", "", seq, err)
	}
	e.partial[seq] = sum
	return sum, nil
}

// Has returns the committed hash for a frame, enabling resume.
func (e *Exporter) Has(seq int64) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	sum, ok := e.partial[seq]
	return sum, ok
}

// RootHash folds the per-frame hashes in frame order into one digest, so a
// resumed run and a complete re-run compare equal.
func (e *Exporter) RootHash() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.partial) == 0 {
		return "", media.New(media.KindLimit, "export.root", "", -1, errors.New("no committed frames"))
	}
	seqs := make([]int64, 0, len(e.partial))
	for seq := range e.partial {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	h := sha256.New()
	for _, seq := range seqs {
		fmt.Fprintf(h, "%d %s\n", seq, e.partial[seq])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Finalize atomically writes the ownership manifest with the final hashes
// after verifying every frame on disk matches the in-memory record.
func (e *Exporter) Finalize(own Ownership) error {
	sums, err := e.verifyOnDisk()
	if err != nil {
		return err
	}
	own.Program = "fieldcut"
	if own.Version == 0 {
		own.Version = 1
	}
	own.Frames = len(sums)
	h := sha256.New()
	for _, seq := range sortedSeqs(sums) {
		fmt.Fprintf(h, "%d %s\n", seq, sums[seq])
	}
	own.Root = hex.EncodeToString(h.Sum(nil))
	data, err := json.MarshalIndent(own, "", "  ")
	if err != nil {
		return media.New(media.KindConflict, "export.finalize", "", -1, err)
	}
	data = append(data, '\n')
	path := filepath.Join(e.dir, ManifestName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return media.New(media.KindConflict, "export.finalize", "", -1, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return media.New(media.KindConflict, "export.finalize", "", -1, err)
	}
	return nil
}

// verifyOnDisk re-hashes every recorded frame; a missing or altered frame
// is a conflict, never silently regenerated.
func (e *Exporter) verifyOnDisk() (map[int64]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[int64]string, len(e.partial))
	for seq, want := range e.partial {
		got, err := hashFile(filepath.Join(e.dir, frameFileName(seq)))
		if err != nil {
			return nil, media.New(media.KindConflict, "export.verify", "", seq, err)
		}
		if got != want {
			return nil, media.Newf(media.KindConflict, "export.verify", "", seq, "frame hash changed on disk")
		}
		out[seq] = got
	}
	return out, nil
}

func sortedSeqs(sums map[int64]string) []int64 {
	seqs := make([]int64, 0, len(sums))
	for seq := range sums {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	return seqs
}

func hashFile(path string) (string, error) {
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
