package samplegen

import (
	"os"
	"path/filepath"
)

// ExpectedRegion is ground truth for self-checking corruption coordinates.
type ExpectedRegion struct {
	Code      string
	FileIndex int
	TrackID   uint32
	Offset    int64
}

// KeyExpectation is one expected native-tick keyframe timestamp.
type KeyExpectation struct {
	FileIndex int
	TrackID   uint32
	Ticks     []int64
}

// GeneratedFile describes one written demo asset.
type GeneratedFile struct {
	Index int
	Name  string
	Path  string
}

// FixtureSet is the complete deterministic demo corpus.
type FixtureSet struct {
	MediaDir  string
	Files     []GeneratedFile
	Regions   []ExpectedRegion
	Keyframes []KeyExpectation
}

// Generate writes exactly six two-track assets under root/media.
func Generate(root string, seed int64) (*FixtureSet, error) {
	dir := filepath.Join(root, "media")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	set := &FixtureSet{MediaDir: dir}

	type job struct {
		name  string
		build func() (*normalBuilt, []ExpectedRegion, []KeyExpectation)
	}
	jobs := []job{
		{name: "01_normal_multitrack.mp4", build: func() (*normalBuilt, []ExpectedRegion, []KeyExpectation) {
			specs := StandardSpecs(30000, 48000)
			b := buildNormal(specs, NormalOptions{Seed: seed})
			return b, nil, normalKeyExpectations(0, specs)
		}},
		{name: "02_editlist_quicktime.mov", build: func() (*normalBuilt, []ExpectedRegion, []KeyExpectation) {
			specs := StandardSpecs(24000, 44100)
			specs[0].IncludeEdit = true
			specs[0].EditMediaTime = 1000
			specs[0].EditDuration = 6000
			specs[1].IncludeEdit = true
			specs[1].EditMediaTime = 512
			specs[1].EditDuration = 6000
			b := buildNormal(specs, NormalOptions{QuickTime: true, UnknownBox: true, Seed: seed})
			return b, nil, normalKeyExpectations(1, specs)
		}},
		{name: "03_fragmented.mp4", build: func() (*normalBuilt, []ExpectedRegion, []KeyExpectation) {
			b := buildFragmented(false, cleanFragTracks())
			return b, nil, fragmentKeyExpectations(2, cleanFragTracks())
		}},
		{name: "04_truncated_box.mp4", build: func() (*normalBuilt, []ExpectedRegion, []KeyExpectation) {
			specs := StandardSpecs(25000, 48000)
			bad := New("zzzz", make([]byte, 32)).Mark("truncated-box")
			b := buildNormal(specs, NormalOptions{
				MDATFirst:   true,
				TruncatedAt: "truncated-box",
				ExtraSTBL:   map[uint32][]*Builder{2: {bad}},
				Seed:        seed,
			})
			regions := []ExpectedRegion{{Code: "truncated_box", FileIndex: 3, TrackID: 2, Offset: int64(b.Marks["truncated-box"])}}
			return b, regions, normalKeyExpectations(3, specs)
		}},
		{name: "05_mdat_eof_sample_offset.mp4", build: func() (*normalBuilt, []ExpectedRegion, []KeyExpectation) {
			specs := StandardSpecs(30000, 48000)
			b := buildNormal(specs, NormalOptions{Seed: seed})
			mdatStart := b.Marks["mdat"]
			patchU32(b.Data, mdatStart, uint32(len(b.Data)-mdatStart+128))
			stcoStart := b.Marks[stcoMarker(2)]
			valueOffset := stcoStart + 16
			patchU32(b.Data, valueOffset, uint32(len(b.Data)+77))
			regions := []ExpectedRegion{
				{Code: "mdat_past_eof", FileIndex: 4, TrackID: 0, Offset: int64(mdatStart)},
				{Code: "sample_offset_out_of_bounds", FileIndex: 4, TrackID: 2, Offset: int64(valueOffset)},
			}
			return b, regions, normalKeyExpectations(4, specs)
		}},
		{name: "06_tfdt_regression_zero_ts.mp4", build: func() (*normalBuilt, []ExpectedRegion, []KeyExpectation) {
			tracks := regressionFragTracks()
			b := buildFragmented(false, tracks)
			regions := []ExpectedRegion{{Code: "tfdt_regression", FileIndex: 5, TrackID: 1, Offset: int64(b.Marks["tfdt-bad"])}}
			return b, regions, fragmentKeyExpectations(5, tracks)
		}},
	}

	for i, j := range jobs {
		built, regions, keys := j.build()
		path := filepath.Join(dir, j.name)
		if err := os.WriteFile(path, built.Data, 0o644); err != nil {
			return nil, err
		}
		set.Files = append(set.Files, GeneratedFile{Index: i, Name: j.name, Path: path})
		set.Regions = append(set.Regions, regions...)
		set.Keyframes = append(set.Keyframes, keys...)
	}
	return set, nil
}

func normalKeyExpectations(fileIndex int, specs []TrackSpec) []KeyExpectation {
	var out []KeyExpectation
	for _, s := range specs {
		k := KeyExpectation{FileIndex: fileIndex, TrackID: s.ID}
		for _, n := range s.Keyframes {
			k.Ticks = append(k.Ticks, int64(n-1)*int64(s.Delta))
		}
		out = append(out, k)
	}
	return out
}

func fragmentKeyExpectations(fileIndex int, tracks []FragTrack) []KeyExpectation {
	var out []KeyExpectation
	for _, t := range tracks {
		k := KeyExpectation{FileIndex: fileIndex, TrackID: t.ID}
		for _, f := range t.Fragments {
			pos := int64(f.BaseTime)
			for i, s := range f.Samples {
				if !s.NonSync {
					k.Ticks = append(k.Ticks, pos)
				}
				_ = i
				pos += int64(s.Duration)
			}
		}
		sortTicks(k.Ticks)
		out = append(out, k)
	}
	return out
}

func regressionFragTracks() []FragTrack {
	video := FragTrack{ID: 1, Handler: "vide", Timescale: 30000, Fragments: []Fragment{
		{BaseTime: 0, Samples: []FragSample{
			{Duration: 1001, Size: 16, NonSync: false},
			{Duration: 1001, Size: 16, NonSync: true},
			{Duration: 1001, Size: 16, NonSync: false},
		}},
		{BaseTime: 500, TfdtMarker: "tfdt-bad", Samples: []FragSample{
			{Duration: 1001, Size: 16, NonSync: false},
			{Duration: 1001, Size: 16, NonSync: true},
		}},
	}}
	audioSamples := []FragSample{
		{Duration: 1024, Size: 8, NonSync: false},
		{Duration: 1024, Size: 8, NonSync: true},
		{Duration: 1024, Size: 8, NonSync: false},
		{Duration: 1024, Size: 8, NonSync: true},
	}
	audio := FragTrack{ID: 2, Handler: "soun", Timescale: 0, Fragments: []Fragment{{BaseTime: 0, Samples: audioSamples}}}
	return []FragTrack{video, audio}
}

func sortTicks(values []int64) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j-1] > values[j]; j-- {
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
}
