package pipeline

import (
	"fmt"
	"os"
	"path/filepath"

	"mediamerge/internal/clock"
	"mediamerge/internal/fault"
	"mediamerge/internal/metadata"
)

// spec describes one generated fixture independently of its container. The
// same spec drives file generation and the expected canonical form used by
// the parse roundtrip metric.
type spec struct {
	Name           string
	Container      string // "mxf" or "mp4"
	ContainerColor *metadata.ColorDesc
	Tracks         []trackSpec
	// EditUnits is the MXF timeline length; ignored for MP4.
	EditUnits int
}

type trackSpec struct {
	ID       int
	Kind     metadata.Kind
	Timebase clock.Timebase
	Duration int64

	Width, Height int
	Rotation      int
	Crop          *metadata.Crop
	Color         metadata.ColorDesc
	// StoredColor is what the fixture writes; Color is the expected track
	// descriptor after parsing. They differ only for corrupt fixtures.
	SampleRate int
	Channels   int
	Keyframes  []int // 1-based sample numbers for MP4, same for MXF edit units
}

func intp(v int) *int { return &v }

func colorDesc(p, t, m uint16, full bool) metadata.ColorDesc {
	return metadata.ColorDesc{
		Primaries: uint16p(p), Transfer: uint16p(t), Matrix: uint16p(m),
		FullRange: boolp(full),
	}
}

func uint16p(v uint16) *uint16 { return &v }
func boolp(v bool) *bool       { return &v }

// standardSpecs returns the three delivery inputs. Values are chosen so all
// audio conversions are integer-exact and every fixture fits in a few KiB.
func standardSpecs() []spec {
	container709 := colorDesc(1, 1, 1, false)
	return []spec{
		{
			Name: "studio_cam.mxf", Container: "mxf", EditUnits: 24,
			ContainerColor: &container709,
			Tracks: []trackSpec{
				{
					ID: 1, Kind: metadata.KindVideo, Timebase: clock.Must(25, 1), Duration: 24,
					Width: 1920, Height: 1080, Rotation: 0,
					Crop:      &metadata.Crop{X: 0, Y: 0, Width: 1920, Height: 1040},
					Color:     colorDesc(9, 1, 9, false), // conflicts with container
					Keyframes: []int{1, 9, 17},
				},
				{
					ID: 2, Kind: metadata.KindAudio, Timebase: clock.Must(48000, 1), Duration: 48000,
					SampleRate: 48000, Channels: 6, Keyframes: []int{1},
				},
			},
		},
		{
			Name: "field_cam.mp4", Container: "mp4",
			Tracks: []trackSpec{
				{
					ID: 1, Kind: metadata.KindVideo, Timebase: clock.Must(25, 1), Duration: 24,
					Width: 1280, Height: 720, Rotation: 90,
					Crop:      &metadata.Crop{X: 0, Y: 0, Width: 1280, Height: 600},
					Color:     colorDesc(1, 1, 1, false),
					Keyframes: []int{1, 9, 17},
				},
				{
					ID: 2, Kind: metadata.KindAudio, Timebase: clock.Must(48000, 1), Duration: 48000,
					SampleRate: 48000, Channels: 2, Keyframes: []int{1},
				},
			},
		},
		{
			Name: "remote_edit.mp4", Container: "mp4",
			ContainerColor: &metadata.ColorDesc{
				Primaries: uint16p(9), Transfer: uint16p(14), Matrix: uint16p(9), FullRange: boolp(false),
			},
			Tracks: []trackSpec{
				{
					ID: 1, Kind: metadata.KindVideo, Timebase: clock.Must(25, 1), Duration: 24,
					Width: 960, Height: 540, Rotation: 270,
					Color:     colorDesc(9, 14, 9, false),
					Keyframes: []int{1, 13},
				},
				{
					ID: 2, Kind: metadata.KindAudio, Timebase: clock.Must(24000, 1), Duration: 24000,
					SampleRate: 24000, Channels: 2, Keyframes: []int{1},
				},
			},
		},
	}
}

// BuildFile creates the fixture bytes for a spec.
func BuildFile(sp spec) ([]byte, error) {
	switch sp.Container {
	case "mp4":
		return buildMP4(sp)
	case "mxf":
		return buildMXF(sp)
	default:
		return nil, fmt.Errorf("pipeline: unknown container %q", sp.Container)
	}
}

func buildMP4(sp spec) ([]byte, error) {
	opts := metadata.WriteOptions{ContainerColor: metaColor(sp.ContainerColor)}
	for _, tr := range sp.Tracks {
		// One sample per second of media: enough timing entries to exercise
		// the index without embedding a full-size mdat in every fixture.
		samples := int(tr.Duration) / 1000
		if samples < 2 {
			samples = 2
		}
		delta := tr.Duration / int64(samples)
		if delta < 1 {
			delta = 1
		}
		opts.Tracks = append(opts.Tracks, metadata.WriteTrack{
			Kind: tr.Kind, Timescale: int(tr.Timebase.Rate.Num), Duration: tr.Duration,
			SampleDelta: delta, SampleCount: samples,
			Width: tr.Width, Height: tr.Height, Matrix: metadata.MatrixForRotation(tr.Rotation),
			Crop: tr.Crop, Color: metaColor(&tr.Color),
			SampleRate: tr.SampleRate, Channels: tr.Channels,
		})
	}
	doc, err := metadata.BuildDocument(opts)
	if err != nil {
		return nil, err
	}
	for i := range doc.Tracks {
		var keys []uint32
		for _, k := range sp.Tracks[i].Keyframes {
			if k > doc.Tracks[i].Track.SampleCount {
				continue
			}
			keys = append(keys, uint32(k))
		}
		if len(keys) == 0 {
			keys = []uint32{1}
		}
		doc.Tracks[i].KeySamples = keys
	}
	return doc.Write()
}

func buildMXF(sp spec) ([]byte, error) {
	doc := &metadata.MXFDocument{
		EditUnitBytes: 128, EditUnits: sp.EditUnits, IndexInterval: 1,
		ContainerColor: sp.ContainerColor,
	}
	for _, tr := range sp.Tracks {
		ticks := make([]int64, sp.EditUnits)
		for i := range ticks {
			if tr.Kind == metadata.KindAudio {
				ticks[i] = int64(i) * (tr.Duration / int64(sp.EditUnits))
			} else {
				ticks[i] = int64(i)
			}
		}
		keys := make([]int, 0, len(tr.Keyframes))
		for _, k := range tr.Keyframes {
			keys = append(keys, k-1)
		}
		doc.Tracks = append(doc.Tracks, metadata.MXFTrack{
			ID: tr.ID, Kind: tr.Kind, Timebase: tr.Timebase, Duration: tr.Duration,
			Width: tr.Width, Height: tr.Height, StoredRot: tr.Rotation,
			Crop: tr.Crop, Color: metaColor(&tr.Color),
			SampleRate: tr.SampleRate, Channels: tr.Channels,
			TimestampTicks: ticks, KeyEditUnits: keys,
		})
	}
	return doc.Write()
}

func metaColor(d *metadata.ColorDesc) metadata.ColorDesc {
	if d == nil {
		return metadata.ColorDesc{}
	}
	return *d
}

// WriteFixtures materializes every standard fixture into dir and returns the
// spec list and paths. Generation is deterministic for a given directory.
func WriteFixtures(dir string) ([]spec, map[string]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fault.Wrap(fault.StageParse, fault.CodeWriteFailed, err, "create fixture directory")
	}
	specs := standardSpecs()
	paths := make(map[string]string, len(specs))
	for _, sp := range specs {
		data, err := BuildFile(sp)
		if err != nil {
			return nil, nil, err
		}
		path := filepath.Join(dir, sp.Name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, nil, fault.Wrap(fault.StageParse, fault.CodeWriteFailed, err, path)
		}
		paths[sp.Name] = path
	}
	return specs, paths, nil
}

// CorruptNames are the accepted --corrupt values.
func CorruptNames() []string {
	return []string{"truncated-box", "truncated-partition", "non-monotonic", "bad-crop"}
}

// WriteCorruptFixture generates one damaged file and returns its path. Each
// case exercises a distinct stage of the pipeline: container parsing, MXF
// partition validation, timestamp ordering and crop resolution.
func WriteCorruptFixture(dir, kind string) (string, error) {
	specs := standardSpecs()
	var target spec
	var path string
	switch kind {
	case "truncated-box":
		target = specs[1]
		data, err := BuildFile(target)
		if err != nil {
			return "", err
		}
		data, err = truncateBoxSize(data, "moov")
		if err != nil {
			return "", err
		}
		// The standard fixtures are sized for streaming, which makes a
		// damaged copy needlessly large. A compact carrier proves the same
		// parser rule in a few KiB.
		if len(data) > 64<<10 {
			data, err = compactTruncatedBox()
			if err != nil {
				return "", err
			}
		}
		path = filepath.Join(dir, "corrupt_truncated_box.mp4")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return "", err
		}
	case "truncated-partition":
		target = specs[0]
		data, err := BuildFile(target)
		if err != nil {
			return "", err
		}
		path = filepath.Join(dir, "corrupt_truncated_partition.mxf")
		if err := os.WriteFile(path, data[:len(data)*3/5], 0o644); err != nil {
			return "", err
		}
	case "non-monotonic":
		target = specs[0]
		doc := &metadata.MXFDocument{EditUnitBytes: 128, EditUnits: 24, IndexInterval: 1}
		ticks := make([]int64, 24)
		for i := range ticks {
			ticks[i] = int64(i)
		}
		ticks[10] = ticks[9] // duplicate: not strictly increasing
		doc.Tracks = []metadata.MXFTrack{{
			ID: 1, Kind: metadata.KindVideo, Timebase: clock.Must(25, 1), Duration: 24,
			Width: 1920, Height: 1080,
			Color:          colorDesc(1, 1, 1, false),
			TimestampTicks: ticks, KeyEditUnits: []int{0, 8, 16},
		}}
		data, err := doc.Write()
		if err != nil {
			return "", err
		}
		path = filepath.Join(dir, "corrupt_non_monotonic.mxf")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return "", err
		}
	case "bad-crop":
		target = specs[0]
		doc := &metadata.MXFDocument{EditUnitBytes: 128, EditUnits: 24, IndexInterval: 1,
			ContainerColor: target.ContainerColor}
		ticks := make([]int64, 24)
		for i := range ticks {
			ticks[i] = int64(i)
		}
		doc.Tracks = []metadata.MXFTrack{{
			ID: 1, Kind: metadata.KindVideo, Timebase: clock.Must(25, 1), Duration: 24,
			Width: 1920, Height: 1080,
			Crop:           &metadata.Crop{X: 0, Y: 0, Width: 5000, Height: 1080},
			Color:          colorDesc(1, 1, 1, false),
			TimestampTicks: ticks, KeyEditUnits: []int{0, 8, 16},
		}}
		data, err := doc.Write()
		if err != nil {
			return "", err
		}
		path = filepath.Join(dir, "corrupt_bad_crop.mxf")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return "", err
		}
	default:
		return "", fault.New(fault.StageArgs, fault.CodeInvalidArgument,
			fmt.Sprintf("unknown corruption %q (want %v)", kind, CorruptNames()))
	}
	_ = target
	return path, nil
}

// compactTruncatedBox builds a small MP4 whose trailing moov size is
// inflated past EOF.
func compactTruncatedBox() ([]byte, error) {
	doc, err := metadata.BuildDocument(metadata.WriteOptions{
		Tracks: []metadata.WriteTrack{{
			Kind: metadata.KindVideo, Timescale: 25, Duration: 4, SampleDelta: 1, SampleCount: 4,
			Width: 320, Height: 240, Matrix: metadata.MatrixForRotation(0),
			Color: colorDesc(1, 1, 1, false),
		}},
	})
	if err != nil {
		return nil, err
	}
	doc.Tracks[0].KeySamples = []uint32{1}
	data, err := doc.Write()
	if err != nil {
		return nil, err
	}
	return truncateBoxSize(data, "moov")
}

// truncateBoxSize inflates a top-level box's declared length so the parser
// must detect that it runs past EOF.
func truncateBoxSize(data []byte, boxType string) ([]byte, error) {
	idx := indexOf(data, []byte(boxType))
	if idx < 4 {
		return nil, fmt.Errorf("pipeline: box %q not found", boxType)
	}
	start := idx - 4
	if start+8 > len(data) {
		return nil, fmt.Errorf("pipeline: box %q header outside file", boxType)
	}
	out := append([]byte(nil), data...)
	out[start] = 0x7f
	out[start+1] = 0xff
	out[start+2] = 0xff
	out[start+3] = 0xff
	return out, nil
}

func indexOf(hay, needle []byte) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
