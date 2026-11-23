package testkit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DemoLayout is the generated demonstration input set.
type DemoLayout struct {
	ManifestPath string
	InitSpec     InitSpec
	Fragments    []FragmentSpec
}

// WriteDemoInputs generates the standard demonstration corpus under work/in
// and returns its explicit manifest. The corpus deliberately contains a
// truncated box (first error offset 4096), a declared mdat overflow, an mfhd
// gap and two tfdt regressions.
func WriteDemoInputs(work string) (*DemoLayout, error) {
	inDir := filepath.Join(work, "in")
	if err := os.MkdirAll(inDir, 0o755); err != nil {
		return nil, err
	}
	initSpec, frags := StandardSampleLayout()
	if _, err := Generate(inDir, initSpec, frags); err != nil {
		return nil, err
	}
	var lines []string
	lines = append(lines, "init init.mp4")
	for i := range frags {
		name := fmt.Sprintf("frag-%03d.m4s", frags[i].Seq)
		lines = append(lines, "media "+name)
	}
	manifestPath := filepath.Join(inDir, "manifest.txt")
	content := "# archivecheck explicit input manifest\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(manifestPath, []byte(content), 0o644); err != nil {
		return nil, err
	}
	return &DemoLayout{ManifestPath: manifestPath, InitSpec: initSpec, Fragments: frags}, nil
}

// FaultLayout writes the corruption corpus used by the fault-chain tests.
type FaultLayout struct {
	Dir          string
	ManifestPath string
	Truncated    string
	BadMdatLen   string
	Offset       int64
}

// WriteFaultInputs generates truncated and invalid-length samples. The
// truncated sample begins with a 4096-byte free box so the following moof
// header sits exactly at byte 4096 and declares more bytes than exist, making
// the reported fault offset exact.
func WriteFaultInputs(work string) (*FaultLayout, error) {
	inDir := filepath.Join(work, "faults")
	if err := os.MkdirAll(inDir, 0o755); err != nil {
		return nil, err
	}
	initSpec := InitSpec{TrackID: 1, Timescale: 90000, Handler: "vide", Width: 1280, Height: 720}
	if err := os.WriteFile(filepath.Join(inDir, "init.mp4"), buildInit(initSpec), 0o644); err != nil {
		return nil, err
	}
	samples := []SampleSpec{{Duration: 3000, Size: 256, Sync: true}, {Duration: 3000, Size: 256, Sync: false}}
	trunc := make([]byte, 4096)
	copy(trunc, boxHeader("free", 4096))
	moofHeader := boxHeader("moof", 8192)
	trunc = append(trunc, moofHeader...)
	trunc = append(trunc, make([]byte, 32)...)
	truncPath := filepath.Join(inDir, "truncated.m4s")
	if err := os.WriteFile(truncPath, trunc, 0o644); err != nil {
		return nil, err
	}
	bad := buildFragment(FragmentSpec{Seq: 2, TrackID: 1, BaseTime: 6000, Samples: samples, BadMdatLen: true})
	badPath := filepath.Join(inDir, "badlength.m4s")
	if err := os.WriteFile(badPath, bad, 0o644); err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(inDir, "manifest.txt")
	content := "# archivecheck fault corpus\ninit init.mp4\nmedia truncated.m4s\nmedia badlength.m4s\n"
	if err := os.WriteFile(manifestPath, []byte(content), 0o644); err != nil {
		return nil, err
	}
	return &FaultLayout{Dir: inDir, ManifestPath: manifestPath, Truncated: truncPath, BadMdatLen: badPath, Offset: 4096}, nil
}

func boxHeader(typ string, size int) []byte {
	b := make([]byte, 8)
	b[0] = byte(size >> 24)
	b[1] = byte(size >> 16)
	b[2] = byte(size >> 8)
	b[3] = byte(size)
	copy(b[4:], typ)
	return b
}

// BuildShortMdatFragment builds a segment whose moof is valid but whose mdat
// payload is too small for the declared sample sizes.
func BuildShortMdatFragment(trackID uint32, baseTime uint64, samples []SampleSpec) []byte {
	return buildFragment(FragmentSpec{
		Seq: 1, TrackID: trackID, BaseTime: baseTime, Samples: samples, ShortMdat: true,
	})
}
