// Package source discovers captured inputs, runs a bounded worker pool and
// derives RTP timestamp corroboration. Discovery is purely filesystem based;
// generated fixtures and real capture trees share the same path conventions.
package source

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mediad/internal/core"
)

// Input kinds.
const (
	KindFragment = "fragment"
	KindInit     = "init"
	KindRTP      = "rtp"
	KindOther    = "other"
)

// Source is one discovered file. Rel is slash-separated and relative to the
// discovery root, so digests and manifests are portable.
type Source struct {
	Abs            string
	Rel            string
	Camera         string
	Representation string
	Kind           string
	Size           int64
	SHA256         string
}

// Plan is the deterministic discovery result.
type Plan struct {
	Root        string
	All         []Source
	Fragments   []Source
	Inits       []Source
	RTP         map[string]Source // fragment Rel -> .rtp sidecar
	InputDigest string
}

func kindOf(rel, name string) string {
	switch {
	case strings.HasSuffix(name, ".m4s"), strings.HasSuffix(name, ".fmp4"):
		return KindFragment
	case name == "init.mp4" || strings.HasSuffix(name, ".init.mp4"):
		return KindInit
	case strings.HasSuffix(name, ".rtp"):
		return KindRTP
	default:
		return KindOther
	}
}

func classifyCameraRep(rel string) (camera, rep string) {
	parts := strings.Split(rel, "/")
	switch len(parts) {
	case 0, 1:
		return "cam01", "default"
	case 2:
		return "cam01", parts[0]
	default:
		return parts[0], parts[1]
	}
}

// Discover walks root, hashes every regular file and builds the deterministic
// input digest. Fragments over core.MaxFragmentBytes are a fatal InputError.
func Discover(root string) (*Plan, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, &core.InputError{Err: err}
	}
	st, err := os.Stat(rootAbs)
	if err != nil {
		return nil, &core.InputError{Err: err}
	}
	if !st.IsDir() {
		return nil, &core.InputError{Err: fmt.Errorf("input root %s is not a directory", rootAbs)}
	}

	plan := &Plan{Root: rootAbs, RTP: map[string]Source{}}
	hashCtx := sha256.New()

	err = filepath.WalkDir(rootAbs, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		relOS, err := filepath.Rel(rootAbs, path)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(relOS)
		info, err := d.Info()
		if err != nil {
			return err
		}
		kind := kindOf(rel, name)
		if kind == KindFragment && info.Size() > core.MaxFragmentBytes {
			return &core.InputError{Err: core.Faultf(core.KindOutOfBounds, -1,
				"fragment %s size %d exceeds 128 MiB ceiling", rel, info.Size())}
		}
		sum, err := hashFile(path)
		if err != nil {
			return err
		}
		camera, rep := classifyCameraRep(rel)
		s := Source{
			Abs: path, Rel: rel, Camera: camera, Representation: rep,
			Kind: kind, Size: info.Size(), SHA256: sum,
		}
		plan.All = append(plan.All, s)

		// Input digest binds path, size and content hash in sorted order.
		hashCtx.Write([]byte(rel))
		hashCtx.Write([]byte{0})
		hashCtx.Write([]byte(sum))
		hashCtx.Write([]byte{0})

		switch kind {
		case KindFragment:
			plan.Fragments = append(plan.Fragments, s)
		case KindInit:
			plan.Inits = append(plan.Inits, s)
		case KindRTP:
			fragRel := strings.TrimSuffix(rel, ".rtp") + ".m4s"
			plan.RTP[fragRel] = s
		}
		return nil
	})
	if err != nil {
		if _, ok := err.(*core.InputError); ok {
			return nil, err
		}
		return nil, &core.InputError{Err: err}
	}

	sortSources(plan.All)
	sortSources(plan.Fragments)
	sortSources(plan.Inits)
	plan.InputDigest = hex.EncodeToString(hashCtx.Sum(nil))
	return plan, nil
}

func sortSources(s []Source) {
	sort.SliceStable(s, func(i, j int) bool { return s[i].Rel < s[j].Rel })
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
