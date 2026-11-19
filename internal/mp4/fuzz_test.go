package mp4_test

import (
	"bytes"
	"os"
	"testing"

	"archivearchivist/internal/mp4"
	"archivearchivist/internal/testkit"
)

// FuzzScan enforces the "damaged input must not crash" contract: any byte
// sequence may produce faults but never a panic.
func FuzzScan(f *testing.F) {
	base := func() []byte {
		path := f.TempDir() + "/seed.mp4"
		samples := make([]testkit.SampleSpec, 8)
		for i := range samples {
			samples[i] = testkit.SampleSpec{Duration: 3000, Keyframe: i%3 == 0, Size: 200}
		}
		if _, err := testkit.WriteMP4(path, testkit.TrackSpec{
			TrackID: 1, Kind: "video", Codec: "avc1", Timescale: 90000,
			Width: 320, Height: 180, Seed: 5, Samples: samples,
		}); err != nil {
			f.Fatal(err)
		}
		b, err := readFile(path)
		if err != nil {
			f.Fatal(err)
		}
		return b
	}()
	f.Add(base)
	f.Add([]byte{})
	f.Add([]byte("not an mp4"))
	f.Add(base[:len(base)/2])

	f.Fuzz(func(t *testing.T, data []byte) {
		sc, err := mp4.ScanReader(bytes.NewReader(data), int64(len(data)), "fuzz.mp4", "F1")
		if err != nil {
			// A returned error is acceptable for unreadable input, but the
			// package contract prefers a scan with faults.
			return
		}
		if sc == nil {
			t.Fatalf("nil scan without error")
		}
		_ = sc.CorruptRanges
	})
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
