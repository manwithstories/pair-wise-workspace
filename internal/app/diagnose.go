package app

import (
	"errors"
	"io"
	"os"

	"github.com/relayteam/relay/internal/manifest"
	"github.com/relayteam/relay/internal/media"
	"github.com/relayteam/relay/internal/segment"
	"github.com/relayteam/relay/internal/timeline"
	"github.com/relayteam/relay/internal/verify"
)

// Diagnosis is the located result of streaming one corrupt fixture through
// parsing, validation, cutting and the manifest error boundary.
type Diagnosis struct {
	Path     string   `json:"path"`
	RecordNo int      `json:"first_bad_record"`
	Code     string   `json:"code"`
	Stages   []string `json:"stages"`
	SHA256   string   `json:"sha256"`
	Located  bool     `json:"located"`
	Err      error    `json:"-"`
}

// DiagnoseFile locates the first bad record and propagates that exact location
// through the cut and manifest packages.
func DiagnoseFile(path, manifestDir string) Diagnosis {
	fp, _ := verify.SHA256File(path)
	d := Diagnosis{Path: path, SHA256: fp}
	f, err := os.Open(path)
	if err != nil {
		d.Err = media.E(media.ErrUnlocatedCorrupt, "app.diagnose", media.Meta{}, err)
		d.Code = media.CodeOf(media.ErrUnlocatedCorrupt)
		return d
	}
	defer f.Close()

	reader := timeline.NewReader(f)
	validator := timeline.NewValidator()
	cutter := segment.New(0)
	var bad *timeline.Record
	var first error
	for first == nil {
		rec, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			first = err
			bad = rec // CRC errors intentionally retain the decoded record.
			break
		}
		if err := validator.Validate(rec); err != nil {
			first = err
			bad = rec
			break
		}
		if _, err := cutter.Assign(*rec); err != nil {
			first = err
			bad = rec
			break
		}
	}
	if first == nil {
		first = media.E(media.ErrUnlocatedCorrupt, "app.diagnose", media.Meta{RecordNo: 0},
			errors.New("input ended without a located corruption"))
	}
	if bad != nil {
		// Exercise the cutting boundary with the same source location. A
		// secondary alignment issue must not replace the first bad record.
		_, _ = cutter.Assign(*bad)
	}
	first = segment.AnnotateError(first)
	first = manifest.ReportCorruption(manifestDir, first)
	d.Err = first
	if me, ok := media.First(first); ok {
		d.RecordNo = me.Meta.RecordNo
		d.Code = me.Code
		d.Stages = append([]string(nil), me.Stages...)
		d.Located = d.RecordNo > 0
	}
	return d
}
