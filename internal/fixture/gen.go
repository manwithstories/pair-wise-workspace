package fixture

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	Timescale        int64 = 90000
	SegmentTicks     int64 = 180000
	DefaultSegmentMS int64 = 2000
)

type Injections struct {
	DriftMS  int64
	Gaps     int
	Overlaps int
	BadTFDT  int
}

type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

func validationError(format string, args ...any) error {
	return &ValidationError{Message: fmt.Sprintf(format, args...)}
}

func (i Injections) Empty() bool {
	return i.DriftMS == 0 && i.Gaps == 0 && i.Overlaps == 0 && i.BadTFDT == 0
}

func ParseInjections(specs []string) (Injections, error) {
	var result Injections
	seen := make(map[string]bool)
	for _, spec := range specs {
		if spec == "" {
			continue
		}
		parts := strings.Split(spec, ":")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return Injections{}, validationError("invalid injection %q, want name:value", spec)
		}
		name := strings.ToLower(parts[0])
		if seen[name] {
			return Injections{}, validationError("duplicate injection %q", name)
		}
		seen[name] = true
		value, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || value < 0 {
			return Injections{}, validationError("injection %q needs a non-negative integer", spec)
		}
		switch name {
		case "drift":
			result.DriftMS = value
		case "gap":
			result.Gaps = int(value)
		case "overlap":
			result.Overlaps = int(value)
		case "bad-tfdt":
			result.BadTFDT = int(value)
		default:
			return Injections{}, validationError("unknown injection %q", name)
		}
	}
	return result, nil
}

type Options struct {
	Out        string
	Seed       int64
	Video      int
	Audio      int
	Segments   int
	Injections Injections
}

type Result struct {
	Representations int
	Segments        int
	Seed            int64
	Out             string
}

type representation struct {
	ID        string
	Kind      string
	Width     uint32
	Height    uint32
	Bandwidth int64
}

func Generate(options Options) (Result, error) {
	if err := validateOptions(options); err != nil {
		return Result{}, err
	}
	reps := representations(options.Video, options.Audio)
	if err := prepareOutput(options.Out); err != nil {
		return Result{}, err
	}
	written := 0
	for repIndex, rep := range reps {
		dir := filepath.Join(options.Out, rep.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(filepath.Join(dir, "init.mp4"),
			buildInit(rep), 0o644); err != nil {
			return Result{}, err
		}
		for segmentIndex := 0; segmentIndex < options.Segments; segmentIndex++ {
			if isGap(options.Injections, repIndex, segmentIndex, len(reps)) {
				continue
			}
			start, duration := segmentTiming(options, repIndex, segmentIndex, len(reps))
			name := fmt.Sprintf("seg-%d.m4s", segmentNameTime(options, repIndex,
				segmentIndex, len(reps)))
			payload := segmentPayload(options.Seed, rep.ID, segmentIndex)
			data := buildSegment(1, start, duration, payload)
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
				return Result{}, err
			}
			written++
		}
	}
	return Result{
		Representations: len(reps), Segments: written,
		Seed: options.Seed, Out: options.Out,
	}, nil
}

func validateOptions(options Options) error {
	if options.Out == "" {
		return validationError("output directory is required")
	}
	if options.Video < 1 || options.Audio < 1 {
		return validationError("video and audio representation counts must be positive")
	}
	if options.Segments < 1 || options.Segments > 100000 {
		return validationError("segments must be between 1 and 100000")
	}
	if options.Injections.DriftMS > 60000 {
		return validationError("drift must not exceed 60000 ms")
	}
	maxEvents := options.Segments - 1
	if maxEvents < 0 {
		maxEvents = 0
	}
	if options.Injections.Gaps > maxEvents || options.Injections.Overlaps > maxEvents ||
		options.Injections.BadTFDT > maxEvents {
		return validationError("injection count exceeds available segments")
	}
	return nil
}

func representations(videoCount, audioCount int) []representation {
	video := []representation{
		{ID: "v1080", Kind: "video", Width: 1920, Height: 1080, Bandwidth: 5000000},
		{ID: "v720", Kind: "video", Width: 1280, Height: 720, Bandwidth: 3000000},
		{ID: "v480", Kind: "video", Width: 854, Height: 480, Bandwidth: 1800000},
		{ID: "v360", Kind: "video", Width: 640, Height: 360, Bandwidth: 1000000},
	}
	for i := len(video); i < videoCount; i++ {
		video = append(video, representation{
			ID: fmt.Sprintf("v%d", 240-i*24), Kind: "video",
			Width: uint32(426 + i*32), Height: uint32(240 + i*24),
			Bandwidth: int64(700000 - i*50000),
		})
	}
	audio := []representation{
		{ID: "a1", Kind: "audio", Bandwidth: 128000},
		{ID: "a2", Kind: "audio", Bandwidth: 96000},
	}
	for i := len(audio); i < audioCount; i++ {
		audio = append(audio, representation{
			ID: fmt.Sprintf("a%d", i+1), Kind: "audio", Bandwidth: 64000,
		})
	}
	return append(video[:videoCount], audio[:audioCount]...)
}

func prepareOutput(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if abs == string(filepath.Separator) {
		return fmt.Errorf("refusing to use filesystem root as output")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if abs == cwd {
		return fmt.Errorf("refusing to replace current working directory")
	}
	if info, statErr := os.Lstat(abs); statErr == nil {
		if !info.IsDir() {
			return fmt.Errorf("output %q is not a directory", dir)
		}
		if err := os.RemoveAll(abs); err != nil {
			return err
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	return os.MkdirAll(abs, 0o755)
}

func isGap(inject Injections, repIndex, segmentIndex, repCount int) bool {
	for event := 0; event < inject.Gaps; event++ {
		if injectionEvent(event, repCount) == repIndex &&
			injectionIndex(event, repCount, 10) == segmentIndex {
			return true
		}
	}
	return false
}

func injectionEvent(event, repCount int) int {
	if repCount <= 0 {
		return 0
	}
	return event % repCount
}

func injectionIndex(event, repCount, base int) int {
	return base + event/repCount*7
}

func segmentTiming(options Options, repIndex, segmentIndex, repCount int) (int64, int64) {
	nominal := int64(segmentIndex) * SegmentTicks
	offsetMS := int64(0)
	if repCount > 1 {
		offsetMS = options.Injections.DriftMS * int64(repIndex) / int64(repCount-1)
	}
	jitterMS := int64(0)
	if segmentIndex > 2 && options.Injections.DriftMS > 0 {
		jitterMS = int64(segmentIndex - 2)
		if jitterMS > options.Injections.DriftMS {
			jitterMS = options.Injections.DriftMS
		}
	}
	start := nominal + (offsetMS+jitterMS)*Timescale/1000
	duration := SegmentTicks
	for event := 0; event < options.Injections.Overlaps; event++ {
		if injectionEvent(event, repCount) == repIndex &&
			injectionIndex(event, repCount, 12) == segmentIndex {
			shift := int64(100) * Timescale / 1000
			start -= shift
			duration += shift
		}
	}
	for event := 0; event < options.Injections.BadTFDT; event++ {
		if injectionEvent(event, repCount) == repIndex &&
			injectionIndex(event, repCount, 14) == segmentIndex {
			start = nominal - SegmentTicks - 1
			duration = SegmentTicks
		}
	}
	return start, duration
}

func segmentNameTime(options Options, repIndex, segmentIndex, repCount int) int64 {
	nominal := int64(segmentIndex) * SegmentTicks
	if repCount <= 1 {
		return nominal
	}
	offsetMS := options.Injections.DriftMS * int64(repIndex) / int64(repCount-1)
	return nominal + offsetMS*Timescale/1000
}

func segmentPayload(seed int64, repID string, segmentIndex int) []byte {
	size := 256
	if strings.HasPrefix(repID, "a") {
		size = 128
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%d", seed, repID, segmentIndex)))
	data := make([]byte, size)
	for i := range data {
		data[i] = sum[i%len(sum)] ^ byte(i*31)
	}
	return data
}

func appendU16(dst []byte, value uint16) []byte {
	var buf [2]byte
	binary.BigEndian.PutUint16(buf[:], value)
	return append(dst, buf[:]...)
}

func appendU32(dst []byte, value uint32) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], value)
	return append(dst, buf[:]...)
}

func appendU64(dst []byte, value uint64) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], value)
	return append(dst, buf[:]...)
}

func join(parts ...[]byte) []byte {
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	result := make([]byte, 0, total)
	for _, part := range parts {
		result = append(result, part...)
	}
	return result
}

func makeBox(typ string, payload []byte) []byte {
	result := make([]byte, 0, 8+len(payload))
	result = appendU32(result, uint32(8+len(payload)))
	result = append(result, typ...)
	return append(result, payload...)
}

func makeFullBox(typ string, version byte, flags uint32, parts ...[]byte) []byte {
	header := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return makeBox(typ, join(append(header, join(parts...)...), nil))
}

func zeros(count int) []byte { return make([]byte, count) }

func identityMatrix() []byte {
	values := []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
	result := make([]byte, 0, 36)
	for _, value := range values {
		result = appendU32(result, value)
	}
	return result
}

func buildInit(rep representation) []byte {
	ftyp := makeBox("ftyp", join([]byte("iso6"), appendU32(nil, 0),
		[]byte("iso6"), []byte("mp41")))
	mvhdPayload := join(
		appendU32(nil, 0), appendU32(nil, 0),
		appendU32(nil, uint32(Timescale)), appendU32(nil, 0),
		appendU32(nil, 0x00010000), appendU16(nil, 0x0100),
		zeros(2), zeros(8), identityMatrix(), zeros(24), appendU32(nil, 2),
	)
	mvhd := makeFullBox("mvhd", 0, 0, mvhdPayload)
	tkhdPayload := join(
		appendU32(nil, 0), appendU32(nil, 0), appendU32(nil, 1),
		appendU32(nil, 0), appendU32(nil, 0), zeros(8),
		appendU16(nil, 0), appendU16(nil, 0),
		appendU16(nil, volumeFor(rep.Kind)), appendU16(nil, 0),
		identityMatrix(), appendU32(nil, rep.Width<<16), appendU32(nil, rep.Height<<16),
	)
	tkhd := makeFullBox("tkhd", 0, 7, tkhdPayload)
	mdhdPayload := join(
		appendU32(nil, 0), appendU32(nil, 0),
		appendU32(nil, uint32(Timescale)), appendU32(nil, 0),
		appendU16(nil, 0x55c4), appendU16(nil, 0),
	)
	mdhd := makeFullBox("mdhd", 0, 0, mdhdPayload)
	handler := "vide"
	handlerName := []byte("video\x00")
	if rep.Kind == "audio" {
		handler = "soun"
		handlerName = []byte("sound\x00")
	}
	hdlr := makeFullBox("hdlr", 0, 0,
		appendU32(nil, 0), []byte(handler), zeros(12), handlerName)
	mediaHeader := makeFullBox("vmhd", 0, 1, zeros(8))
	if rep.Kind == "audio" {
		mediaHeader = makeFullBox("smhd", 0, 0, zeros(4))
	}
	url := makeFullBox("url ", 0, 1)
	dref := makeFullBox("dref", 0, 0, appendU32(nil, 1), url)
	dinf := makeBox("dinf", dref)
	stsd := makeFullBox("stsd", 0, 0, appendU32(nil, 0))
	stts := makeFullBox("stts", 0, 0, appendU32(nil, 0))
	stsc := makeFullBox("stsc", 0, 0, appendU32(nil, 0))
	stsz := makeFullBox("stsz", 0, 0, appendU32(nil, 0), appendU32(nil, 0))
	stco := makeFullBox("stco", 0, 0, appendU32(nil, 0))
	stbl := makeBox("stbl", join(stsd, stts, stsc, stsz, stco))
	minf := makeBox("minf", join(mediaHeader, dinf, stbl))
	mdia := makeBox("mdia", join(mdhd, hdlr, minf))
	trak := makeBox("trak", join(tkhd, mdia))
	trex := makeFullBox("trex", 0, 0,
		appendU32(nil, 1), appendU32(nil, 1), appendU32(nil, 0),
		appendU32(nil, 0), appendU32(nil, 0))
	mvex := makeBox("mvex", trex)
	moov := makeBox("moov", join(mvhd, trak, mvex))
	return join(ftyp, moov)
}

func volumeFor(kind string) uint16 {
	if kind == "audio" {
		return 0x0100
	}
	return 0
}

func buildSegment(trackID uint32, start, duration int64, payload []byte) []byte {
	if duration < 0 || duration > math.MaxUint32 || start < 0 || start > math.MaxInt64 {
		panic("fixture segment timing out of range")
	}
	styp := makeBox("styp", join([]byte("msdh"), appendU32(nil, 0),
		[]byte("msdh"), []byte("msix")))
	mdat := makeBox("mdat", payload)
	moof := makeMoof(0, uint32(duration), uint32(len(payload)), start)
	dataOffset := int64(len(moof) + 8)
	if dataOffset > math.MaxInt32 {
		panic("fixture moof is too large")
	}
	moof = makeMoof(int32(dataOffset), uint32(duration), uint32(len(payload)), start)
	referenceSize := uint32(len(moof) + len(mdat))
	earliest := uint32(start)
	sidxPayload := join(
		appendU32(nil, 1), appendU32(nil, uint32(Timescale)),
		appendU32(nil, earliest), appendU32(nil, 0),
		appendU16(nil, 0), appendU16(nil, 1),
		appendU32(nil, referenceSize), appendU32(nil, uint32(duration)),
		appendU32(nil, 0x90000001),
	)
	sidx := makeFullBox("sidx", 0, 0, sidxPayload)
	return join(styp, sidx, moof, mdat)
}

func makeMoof(dataOffset int32, duration, sampleSize uint32, start int64) []byte {
	mfhd := makeFullBox("mfhd", 0, 0, appendU32(nil, 1))
	tfhd := makeFullBox("tfhd", 0, 0x020008,
		appendU32(nil, 1), appendU32(nil, duration))
	tfdt := makeFullBox("tfdt", 1, 0, appendU64(nil, uint64(start)))
	trunPayload := join(
		appendU32(nil, 1), appendU32(nil, uint32(dataOffset)),
		appendU32(nil, duration), appendU32(nil, sampleSize),
		appendU32(nil, 0x02000000),
	)
	trun := makeFullBox("trun", 0, 0x000701, trunPayload)
	traf := makeBox("traf", join(tfhd, tfdt, trun))
	return makeBox("moof", join(mfhd, traf))
}
