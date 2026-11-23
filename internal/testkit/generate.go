package testkit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Segment is one file written by the generator.
type Segment struct {
	Name     string
	Path     string
	SHA256   string
	Bytes    int
	Init     bool
	Fragment *FragmentSpec
}

// Manifest is a generated input set plus its digest.
type Manifest struct {
	Dir      string
	Segments []Segment
	Digest   string
}

// StandardSampleLayout builds the canonical demonstration: 12 fragments on
// one video track (48 samples of 4 each) with two tfdt regressions and one
// missing mfhd sequence number.
func StandardSampleLayout() (InitSpec, []FragmentSpec) {
	init := InitSpec{TrackID: 1, Timescale: 90000, Handler: "vide", Width: 1920, Height: 1080}
	var frags []FragmentSpec
	for i := 0; i < 12; i++ {
		base := uint64(i) * 12000
		// Fragments 4 and 9 carry a backwards tfdt that must be repaired.
		if i == 4 {
			base = uint64(i-1) * 12000
		}
		if i == 9 {
			base = uint64(i-2) * 12000
		}
		samples := make([]SampleSpec, 4)
		for s := range samples {
			samples[s] = SampleSpec{Duration: 3000, Size: 512, Sync: s == 0, CTO: 0}
		}
		seq := uint32(i + 1)
		if i >= 6 {
			// Simulate an archive mfhd gap: sequence 7 is absent, so the
			// seventh fragment is numbered 8 and later ones shift by one.
			seq = uint32(i + 2)
		}
		frags = append(frags, FragmentSpec{
			Seq:      seq,
			TrackID:  1,
			BaseTime: base,
			Samples:  samples,
		})
	}
	return init, frags
}

// Generate writes an init segment and fragments with explicit names plus a
// manifest listing them in archive order.
func Generate(dir string, init InitSpec, frags []FragmentSpec) (*Manifest, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	m := &Manifest{Dir: dir}
	write := func(name string, data []byte, isInit bool, fs *FragmentSpec) error {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		m.Segments = append(m.Segments, Segment{
			Name: name, Path: path, SHA256: hex.EncodeToString(sum[:]),
			Bytes: len(data), Init: isInit, Fragment: fs,
		})
		return nil
	}
	if err := write("init.mp4", buildInit(init), true, nil); err != nil {
		return nil, err
	}
	for i := range frags {
		name := fmt.Sprintf("frag-%03d.m4s", frags[i].Seq)
		fs := frags[i]
		if err := write(name, buildFragment(fs), false, &fs); err != nil {
			return nil, err
		}
	}
	h := sha256.New()
	for _, s := range m.Segments {
		fmt.Fprintf(h, "%s %s\n", s.Name, s.SHA256)
	}
	m.Digest = hex.EncodeToString(h.Sum(nil))
	return m, nil
}

// Names returns the segment file names in manifest order.
func (m *Manifest) Names() []string {
	out := make([]string, len(m.Segments))
	for i, s := range m.Segments {
		out[i] = s.Name
	}
	return out
}
