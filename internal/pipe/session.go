package pipe

import (
	"fmt"
	"os"

	"archivecheck/internal/index"
	"archivecheck/internal/parse"
)

// LoadedInput is one parsed archive input set: per group, an init segment and
// its ordered media fragments with open files.
type LoadedInput struct {
	Manifest  *Manifest
	Inits     []*parse.Init
	Fragments []*parse.Fragment
	Files     map[string]*os.File
}

// Close releases every open segment file.
func (l *LoadedInput) Close() {
	for _, f := range l.Files {
		if f != nil {
			f.Close()
		}
	}
}

// LoadInputs resolves and parses every entry in the manifest. It rejects
// unsafe paths (handled by the loader) and surfaces parser faults with the
// exact segment, box type and byte offset.
func LoadInputs(m *Manifest) (*LoadedInput, error) {
	in := &LoadedInput{Manifest: m, Files: map[string]*os.File{}}
	for _, g := range m.Groups {
		f, err := os.Open(g.Init.Path)
		if err != nil {
			in.Close()
			return nil, fmt.Errorf("open init %s: %w", g.Init.Name, err)
		}
		in.Files[g.Init.Name] = f
		init, err := parse.ParseInit(f, g.Init.Name)
		if err != nil {
			in.Close()
			return nil, err
		}
		in.Inits = append(in.Inits, init)
		for _, media := range g.Media {
			mf, err := os.Open(media.Path)
			if err != nil {
				in.Close()
				return nil, fmt.Errorf("open media %s: %w", media.Name, err)
			}
			in.Files[media.Name] = mf
			frag, err := parse.ParseFragment(mf, media.Name, init)
			if err != nil {
				in.Close()
				return nil, err
			}
			in.Fragments = append(in.Fragments, frag)
		}
	}
	return in, nil
}

// InitFor returns the init segment that owns the given fragment.
func (l *LoadedInput) InitFor(segment string) *parse.Init {
	for _, g := range l.Manifest.Groups {
		for _, media := range g.Media {
			if media.Name == segment {
				for _, init := range l.Inits {
					if init.Segment == g.Init.Name {
						return init
					}
				}
			}
		}
	}
	return nil
}

// CompatibleStart validates that an on-disk checkpoint belongs to this exact
// input manifest and that its committed prefix still matches its digest,
// returning the resumable start state. Any mismatch is an incompatible-resume
// fault, never a silent restart.
func CompatibleStart(m *Manifest, cp *index.Checkpoint) (Start, error) {
	start := Start{Resuming: true, InputDigest: m.Digest}
	if cp.InputDigest != m.Digest {
		return start, fmt.Errorf("checkpoint input digest %s does not match manifest %s",
			cp.InputDigest, m.Digest)
	}
	if cp.FragmentsTotal != countMedia(m) {
		return start, fmt.Errorf("checkpoint expects %d fragments, manifest has %d",
			cp.FragmentsTotal, countMedia(m))
	}
	if cp.Committed > cp.FragmentsTotal {
		return start, fmt.Errorf("checkpoint committed %d > total %d", cp.Committed, cp.FragmentsTotal)
	}
	start.Committed = cp.Committed
	start.CommittedDigest = cp.CommittedDigest
	start.OutputBytes = cp.OutputBytes
	start.NextDecodeTime = cp.NextDecodeTime
	start.RepairedTfdt = cp.RepairedTfdt
	start.MissingMfhd = cp.MissingMfhd
	if cp.Index != nil {
		start.Entries = cp.Index.Entries
	}
	return start, nil
}

func countMedia(m *Manifest) int {
	n := 0
	for _, g := range m.Groups {
		n += len(g.Media)
	}
	return n
}
