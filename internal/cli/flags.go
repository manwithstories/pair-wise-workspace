// Package cli implements the mp4probe subcommands: argument parsing,
// timecode handling, JSON output (stdout and/or file) and exit codes.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"mp4probe/internal/box"
	"mp4probe/internal/export"
	"mp4probe/internal/fixture"
	"mp4probe/internal/index"
	"mp4probe/internal/moov"
	"mp4probe/internal/seek"
	"mp4probe/internal/timeline"
)

// Exit codes.
const (
	ExitOK          = 0
	ExitUsage       = 2 // parameter errors
	ExitStructure   = 3 // box structure errors
	ExitConsistency = 4 // index self-consistency failures
	ExitLocate      = 5 // no locatable keyframe / out of range
	ExitMismatch    = 6 // hash mismatch
	ExitIO          = 7 // read/write errors
)

// ExitError carries a specific exit code and structured fields.
type ExitError struct {
	Code   int
	Msg    string
	Fields map[string]any
}

func (e *ExitError) Error() string { return e.Msg }

// Errorf builds an ExitError.
func Errorf(code int, format string, args ...any) *ExitError {
	return &ExitError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// ExitCode maps an error to its process exit code.
func ExitCode(err error) int {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	switch {
	case errors.Is(err, box.ErrStructure):
		return ExitStructure
	case errors.Is(err, index.ErrInconsistent):
		return ExitConsistency
	case errors.Is(err, seek.ErrLocate):
		return ExitLocate
	default:
		return ExitIO
	}
}

// PrintError writes a JSON error object to w.
func PrintError(w io.Writer, err error) {
	code := ExitCode(err)
	obj := map[string]any{"error": err.Error(), "code": code}
	var ee *ExitError
	if errors.As(err, &ee) {
		for k, v := range ee.Fields {
			obj[k] = v
		}
	}
	raw, _ := json.Marshal(obj)
	fmt.Fprintf(w, "%s\n", raw)
}

// ValidatePath rejects absolute paths and paths escaping the working
// directory via "..". All file arguments pass through it.
func ValidatePath(p string) error {
	if p == "" {
		return Errorf(ExitUsage, "empty path")
	}
	if filepath.IsAbs(p) {
		return Errorf(ExitUsage, "absolute paths are not allowed: %q", p)
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return Errorf(ExitUsage, "path escapes working directory: %q", p)
	}
	return nil
}

// ParseTimecode parses "HH:MM:SS.mmm" (fraction optional) into
// milliseconds.
func ParseTimecode(s string) (int64, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, Errorf(ExitUsage, "invalid timecode %q: want HH:MM:SS.mmm", s)
	}
	hours, err := strconv.Atoi(parts[0])
	if err != nil || hours < 0 {
		return 0, Errorf(ExitUsage, "invalid timecode %q: bad hours", s)
	}
	mins, err := strconv.Atoi(parts[1])
	if err != nil || mins < 0 || mins > 59 {
		return 0, Errorf(ExitUsage, "invalid timecode %q: bad minutes", s)
	}
	secParts := strings.SplitN(parts[2], ".", 2)
	secs, err := strconv.Atoi(secParts[0])
	if err != nil || secs < 0 || secs > 59 {
		return 0, Errorf(ExitUsage, "invalid timecode %q: bad seconds", s)
	}
	ms := int64((hours*60+mins)*60+secs) * 1000
	if len(secParts) == 2 {
		frac := secParts[1]
		if len(frac) == 0 || len(frac) > 3 {
			return 0, Errorf(ExitUsage, "invalid timecode %q: bad fraction", s)
		}
		for len(frac) < 3 {
			frac += "0"
		}
		f, err := strconv.Atoi(frac)
		if err != nil {
			return 0, Errorf(ExitUsage, "invalid timecode %q: bad fraction", s)
		}
		ms += int64(f)
	}
	return ms, nil
}

// FormatTimecode renders milliseconds as HH:MM:SS.mmm.
func FormatTimecode(ms int64) string {
	h := ms / 3600000
	m := (ms / 60000) % 60
	s := (ms / 1000) % 60
	milli := ms % 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, milli)
}

// WriteJSON marshals v as compact JSON to w.
func WriteJSON(w io.Writer, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", raw)
	return err
}

// WriteJSONFile writes v as compact JSON to a new file.
func WriteJSONFile(path string, v any) error {
	if err := ValidatePath(path); err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return WriteJSON(f, v)
}

// openInput validates and opens an input MP4.
func openInput(path string) (*os.File, int64, error) {
	if err := ValidatePath(path); err != nil {
		return nil, 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

// loadIndex parses the file and builds a validated in-memory index.
func loadIndex(path string) (*index.Index, *os.File, error) {
	f, size, err := openInput(path)
	if err != nil {
		return nil, nil, err
	}
	m, err := moov.Parse(f, uint64(size))
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	idx, err := index.Build(path, uint64(size), m)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if err := idx.Validate(); err != nil {
		f.Close()
		return nil, nil, err
	}
	return idx, f, nil
}

// TrackSummary is the per-track index summary.
type TrackSummary struct {
	TrackID         uint32 `json:"track_id"`
	Codec           string `json:"codec"`
	Timescale       uint32 `json:"timescale"`
	Duration        uint64 `json:"duration"`
	MediaDuration   uint64 `json:"media_duration"`
	SampleCount     int    `json:"sample_count"`
	SyncSampleCount int    `json:"sync_sample_count"`
	ChunkCount      int    `json:"chunk_count"`
	ElstSegments    int    `json:"elst_segments"`
	ElstConflict    bool   `json:"elst_conflict"`
	WrapEvents      int    `json:"wrap_events"`
}

// Summary is the index command's stdout report.
type Summary struct {
	File         string         `json:"file"`
	Size         uint64         `json:"size"`
	Moov         map[string]any `json:"moov"`
	Mdat         map[string]any `json:"mdat"`
	UnknownBoxes int            `json:"unknown_boxes"`
	Tracks       []TrackSummary `json:"tracks"`
}

func summarize(idx *index.Index) *Summary {
	s := &Summary{
		File:         idx.File,
		Size:         idx.Size,
		Moov:         map[string]any{"offset": idx.MoovOffset, "traks": len(idx.Tracks)},
		Mdat:         map[string]any{"offset": idx.MdatOffset, "end": idx.MdatEnd},
		UnknownBoxes: idx.UnknownBoxes,
	}
	for _, t := range idx.Tracks {
		s.Tracks = append(s.Tracks, TrackSummary{
			TrackID: t.TrackID, Codec: t.Codec, Timescale: t.Timescale,
			Duration: t.Duration, MediaDuration: t.MediaDuration,
			SampleCount: t.SampleCount, SyncSampleCount: t.SyncSampleCount,
			ChunkCount: t.ChunkCount, ElstSegments: t.ElstSegments,
			ElstConflict: t.ElstConflict, WrapEvents: t.WrapEvents,
		})
	}
	return s
}

// RunFixture implements the fixture subcommand.
func RunFixture(args []string) error {
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	out := fs.String("out", "", "output directory (required)")
	cases := fs.String("cases", "", "comma-separated case list (required)")
	seed := fs.Int64("seed", 1, "deterministic seed")
	asJSON := fs.Bool("json", false, "print JSON to stdout")
	if err := fs.Parse(args); err != nil {
		return Errorf(ExitUsage, "%v", err)
	}
	if *out == "" || *cases == "" {
		return &ExitError{Code: ExitUsage, Msg: "fixture requires --out and --cases",
			Fields: map[string]any{"missing": []string{"out", "cases"}}}
	}
	if err := ValidatePath(*out); err != nil {
		return err
	}
	list := strings.Split(*cases, ",")
	for _, c := range list {
		ok := false
		for _, known := range fixture.Cases {
			if c == known {
				ok = true
			}
		}
		if !ok {
			return &ExitError{Code: ExitUsage, Msg: "unknown fixture case: " + c,
				Fields: map[string]any{"case": c}}
		}
	}
	paths, err := fixture.Generate(*out, list, *seed, fixture.Params{})
	if err != nil {
		return err
	}
	if *asJSON {
		return WriteJSON(os.Stdout, map[string]any{"written": paths, "seed": *seed})
	}
	for _, p := range paths {
		fmt.Fprintf(os.Stderr, "written %s\n", p)
	}
	return nil
}

// RunIndex implements the index subcommand.
func RunIndex(args []string) error {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	in := fs.String("in", "", "input MP4 (required)")
	out := fs.String("out", "", "write full index JSON to this file")
	asJSON := fs.Bool("json", false, "print summary JSON to stdout")
	if err := fs.Parse(args); err != nil {
		return Errorf(ExitUsage, "%v", err)
	}
	if *in == "" {
		return &ExitError{Code: ExitUsage, Msg: "index requires --in",
			Fields: map[string]any{"missing": []string{"in"}}}
	}
	idx, f, err := loadIndex(*in)
	if err != nil {
		return err
	}
	defer f.Close()
	if *out != "" {
		if err := ValidatePath(*out); err != nil {
			return err
		}
		if dir := filepath.Dir(*out); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		w, err := os.Create(*out)
		if err != nil {
			return err
		}
		if err := index.Save(idx, w); err != nil {
			w.Close()
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
	}
	if *asJSON {
		return WriteJSON(os.Stdout, summarize(idx))
	}
	s := summarize(idx)
	fmt.Printf("file=%s size=%d moov.traks=%d unknown_boxes=%d\n",
		s.File, s.Size, len(s.Tracks), s.UnknownBoxes)
	for _, t := range s.Tracks {
		fmt.Printf("track_id=%d timescale=%d duration=%d media_duration=%d codec=%s "+
			"sample_count=%d sync_sample_count=%d chunk_count=%d elst_segments=%d "+
			"elst_conflict=%v wrap_events=%d\n",
			t.TrackID, t.Timescale, t.Duration, t.MediaDuration, t.Codec,
			t.SampleCount, t.SyncSampleCount, t.ChunkCount, t.ElstSegments,
			t.ElstConflict, t.WrapEvents)
	}
	return nil
}

// trackMapper rebuilds the edit list mapper for one indexed track.
func trackMapper(tr *index.TrackIndex) *timeline.Mapper {
	return timeline.NewMapper(tr.Elst, tr.MovieTimescale, tr.Timescale, tr.ElstConflict)
}

// requireTrack returns the track index or a usage error.
func requireTrack(idx *index.Index, trackID uint32) (*index.TrackIndex, error) {
	tr := idx.Track(trackID)
	if tr == nil {
		return nil, &ExitError{Code: ExitUsage,
			Msg:    fmt.Sprintf("track %d not found", trackID),
			Fields: map[string]any{"track": trackID}}
	}
	return tr, nil
}

// RunSeek implements the seek subcommand.
func RunSeek(args []string) error {
	fs := flag.NewFlagSet("seek", flag.ContinueOnError)
	in := fs.String("in", "", "input MP4 (required)")
	at := fs.String("at", "", "target timecode HH:MM:SS.mmm (required)")
	track := fs.Uint("track", 1, "track ID")
	frames := fs.Int("frames", 8, "frames to hash from the landing frame")
	out := fs.String("out", "", "also write result JSON to this file")
	asJSON := fs.Bool("json", false, "print JSON to stdout")
	if err := fs.Parse(args); err != nil {
		return Errorf(ExitUsage, "%v", err)
	}
	if *in == "" || *at == "" {
		return &ExitError{Code: ExitUsage, Msg: "seek requires --in and --at",
			Fields: map[string]any{"missing": []string{"in", "at"}}}
	}
	if *frames <= 0 {
		return Errorf(ExitUsage, "--frames must be positive")
	}
	ms, err := ParseTimecode(*at)
	if err != nil {
		return err
	}
	idx, f, err := loadIndex(*in)
	if err != nil {
		return err
	}
	defer f.Close()
	tr, err := requireTrack(idx, uint32(*track))
	if err != nil {
		return err
	}
	mapper := trackMapper(tr)
	loc, err := seek.NewLocator(idx, tr.TrackID, mapper, f)
	if err != nil {
		return err
	}
	targetPres := ms * int64(tr.MovieTimescale) / 1000
	res, err := loc.Locate(targetPres, *frames)
	if err != nil {
		return err
	}
	res.RequestedPts = ms * int64(tr.Timescale) / 1000
	if *asJSON {
		if err := WriteJSON(os.Stdout, res); err != nil {
			return err
		}
	} else {
		fmt.Printf("requested_pts=%d landing_pts=%d landing_dts=%d frame_type=%s "+
			"sample_offset=%d sample_size=%d sha256=%s decode_path_length=%d\n",
			res.RequestedPts, res.LandingPts, res.LandingDts, res.FrameType,
			res.SampleOffset, res.SampleSize, res.SHA256, res.DecodePathLength)
		for _, fr := range res.Frames {
			fmt.Printf("sample_index=%d pts=%d dts=%d frame_type=%s offset=%d size=%d sha256=%s\n",
				fr.SampleIndex, fr.PTS, fr.DTS, fr.FrameType, fr.Offset, fr.Size, fr.SHA256)
		}
	}
	if *out != "" {
		return WriteJSONFile(*out, res)
	}
	return nil
}

// RunExport implements the export subcommand.
func RunExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	in := fs.String("in", "", "input MP4 (required)")
	from := fs.String("from", "", "range start timecode (required)")
	to := fs.String("to", "", "range end timecode (required)")
	track := fs.Uint("track", 1, "track ID")
	out := fs.String("out", "", "write manifest JSON to this file")
	asJSON := fs.Bool("json", false, "print JSON to stdout")
	if err := fs.Parse(args); err != nil {
		return Errorf(ExitUsage, "%v", err)
	}
	if *in == "" || *from == "" || *to == "" {
		return &ExitError{Code: ExitUsage, Msg: "export requires --in, --from and --to",
			Fields: map[string]any{"missing": []string{"in", "from", "to"}}}
	}
	fromMs, err := ParseTimecode(*from)
	if err != nil {
		return err
	}
	toMs, err := ParseTimecode(*to)
	if err != nil {
		return err
	}
	idx, f, err := loadIndex(*in)
	if err != nil {
		return err
	}
	defer f.Close()
	tr, err := requireTrack(idx, uint32(*track))
	if err != nil {
		return err
	}
	mapper := trackMapper(tr)
	fromPres := fromMs * int64(tr.MovieTimescale) / 1000
	toPres := toMs * int64(tr.MovieTimescale) / 1000
	m, err := export.Range(idx, tr.TrackID, mapper, f, fromPres, toPres, *from, *to)
	if err != nil {
		return err
	}
	if *asJSON {
		if err := WriteJSON(os.Stdout, m); err != nil {
			return err
		}
	} else {
		fmt.Printf("file=%s track_id=%d from=%s to=%s count=%d\n",
			m.File, m.TrackID, m.From, m.To, m.Count)
		for _, s := range m.Samples {
			fmt.Printf("sample_index=%d pts=%d dts=%d frame_type=%s offset=%d size=%d sha256=%s\n",
				s.SampleIndex, s.PTS, s.DTS, s.FrameType, s.Offset, s.Size, s.SHA256)
		}
	}
	if *out != "" {
		return WriteJSONFile(*out, m)
	}
	return nil
}

// RunVerify implements the verify subcommand.
func RunVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	in := fs.String("in", "", "input MP4 (required)")
	indexPath := fs.String("index", "", "index JSON file (required)")
	expect := fs.String("expect", "", "expected manifest JSON (required)")
	asJSON := fs.Bool("json", false, "print JSON to stdout")
	if err := fs.Parse(args); err != nil {
		return Errorf(ExitUsage, "%v", err)
	}
	if *in == "" || *indexPath == "" || *expect == "" {
		return &ExitError{Code: ExitUsage, Msg: "verify requires --in, --index and --expect",
			Fields: map[string]any{"missing": []string{"in", "index", "expect"}}}
	}
	f, _, err := openInput(*in)
	if err != nil {
		return err
	}
	defer f.Close()
	// The index must load and be self-consistent.
	if err := ValidatePath(*indexPath); err != nil {
		return err
	}
	ir, err := os.Open(*indexPath)
	if err != nil {
		return err
	}
	idx, err := index.Load(ir)
	ir.Close()
	if err != nil {
		return err
	}
	if err := idx.Validate(); err != nil {
		return err
	}
	// Load the expected manifest.
	if err := ValidatePath(*expect); err != nil {
		return err
	}
	er, err := os.Open(*expect)
	if err != nil {
		return err
	}
	var manifest export.Manifest
	dec := json.NewDecoder(er)
	if err := dec.Decode(&manifest); err != nil {
		er.Close()
		return Errorf(ExitUsage, "bad manifest %s: %v", *expect, err)
	}
	er.Close()
	res, err := export.Verify(f, &manifest)
	if err != nil {
		return err
	}
	if *asJSON {
		if err := WriteJSON(os.Stdout, res); err != nil {
			return err
		}
	} else {
		fmt.Printf("checked_samples=%d mismatches=%d\n", res.CheckedSamples, res.Mismatches)
		for _, mm := range res.MismatchSamples {
			fmt.Printf("mismatch sample_index=%d expected=%s actual=%s\n",
				mm.SampleIndex, mm.Expected, mm.Actual)
		}
	}
	if res.Mismatches > 0 {
		return &ExitError{Code: ExitMismatch,
			Msg:    fmt.Sprintf("%d hash mismatches", res.Mismatches),
			Fields: map[string]any{"mismatches": res.Mismatches}}
	}
	return nil
}
