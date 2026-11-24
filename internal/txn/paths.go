package txn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"mediarig/internal/media"
)

// Params are persisted at run start and reverified during resume.
type Params struct {
	Case            string  `json:"case"`
	Root            string  `json:"root"`
	Rate            int     `json:"rate"`
	Layout          string  `json:"layout"`
	LUFS            float64 `json:"lufs"`
	TargetKbps      float64 `json:"target_kbps"`
	Drop            int     `json:"drop"`
	Reorder         int     `json:"reorder"`
	WrapSeq         bool    `json:"wrap_seq"`
	WrapTS          bool    `json:"wrap_ts"`
	ColorConflict   bool    `json:"color_conflict"`
	TruncateLast    bool    `json:"truncate_last"`
	FailAfter       string  `json:"fail_after"`
	DurationPackets int     `json:"duration_packets"`
}

func (p Params) Digest() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", media.InternalWrap(media.StageWAL, err, "encode params")
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Paths is the stable on-disk batch layout.
type Paths struct {
	Root        string
	InputDir    string
	InputFile   string
	SourceJSON  string
	WALDir      string
	WAL         string
	StateDir    string
	Params      string
	Committed   string
	WorkDir     string
	CaptureJSON string
	AudioPCM    string
	AudioJSON   string
	ColorJSON   string
	MP4JSON     string
	OutputDir   string
	MP4         string
	ReportDir   string
	Manifest    string
	Stats       string
	ErrorJSON   string
}

func Layout(root string) (*Paths, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, media.InternalWrap(media.StageInput, err, "resolve root %s", root)
	}
	clean := filepath.Clean(abs)
	p := &Paths{Root: clean}
	p.InputDir = filepath.Join(clean, "input")
	p.InputFile = filepath.Join(p.InputDir, "stream.mrr")
	p.SourceJSON = filepath.Join(p.InputDir, "source.json")
	p.WALDir = filepath.Join(clean, "wal")
	p.WAL = filepath.Join(p.WALDir, WALFileName)
	p.StateDir = filepath.Join(clean, "state")
	p.Params = filepath.Join(p.StateDir, "params.json")
	p.Committed = filepath.Join(p.StateDir, "COMMITTED")
	p.WorkDir = filepath.Join(clean, "work")
	p.CaptureJSON = filepath.Join(p.WorkDir, "capture.json")
	p.AudioPCM = filepath.Join(p.WorkDir, "audio.s16le")
	p.AudioJSON = filepath.Join(p.WorkDir, "audio.json")
	p.ColorJSON = filepath.Join(p.WorkDir, "color.json")
	p.MP4JSON = filepath.Join(p.WorkDir, "mp4-validation.json")
	p.OutputDir = filepath.Join(clean, "output")
	p.MP4 = filepath.Join(p.OutputDir, "master.mp4")
	p.ReportDir = filepath.Join(clean, "report")
	p.Manifest = filepath.Join(p.ReportDir, "manifest.json")
	p.Stats = filepath.Join(p.ReportDir, "stats.env")
	p.ErrorJSON = filepath.Join(p.ReportDir, "last-error.json")
	return p, nil
}

func (p *Paths) Ensure() error {
	for _, d := range []string{p.InputDir, p.WALDir, p.StateDir, p.WorkDir, p.OutputDir, p.ReportDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return media.InternalWrap(media.StageInput, err, "create %s", d)
		}
	}
	return nil
}

// WithinRoot rejects paths that escape the batch root after cleaning.
func WithinRoot(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	return rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

// RejectSymlinkOutput rejects an existing symlink at outputPath. Parent
// symlinks are resolved by callers through absolute root creation.
func RejectSymlinkOutput(outputPath string) error {
	fi, err := os.Lstat(outputPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return media.InternalWrap(media.StageInput, err, "lstat output")
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return media.Policy(media.StageInput, "output path is a symlink: %s", outputPath)
	}
	return nil
}

// CheckDistinctInputOutput rejects identical input/output paths.
func CheckDistinctInputOutput(input, output string) error {
	ai, err := filepath.Abs(input)
	if err != nil {
		return media.InternalWrap(media.StageInput, err, "resolve input")
	}
	ao, err := filepath.Abs(output)
	if err != nil {
		return media.InternalWrap(media.StageInput, err, "resolve output")
	}
	if filepath.Clean(ai) == filepath.Clean(ao) {
		return media.Policy(media.StageInput, "input and output paths must differ: %s", ai)
	}
	return nil
}

func WriteJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return media.InternalWrap(media.StageReport, err, "encode JSON %s", path)
	}
	b = append(b, '\n')
	return WriteAtomic(path, b)
}

func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return media.InternalWrap(media.StageRecover, err, "read %s", path)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return media.CorruptWrap(media.StageRecover, err, "invalid JSON %s", path)
	}
	return nil
}
