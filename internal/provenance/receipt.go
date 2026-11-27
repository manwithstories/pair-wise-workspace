// Package provenance builds the tamper-evident delivery receipt that lets the
// edit team trace every published segment back to its source camera. A receipt
// records the original input digest, the rational time mapping onto the shared
// timeline, the FFmpeg tool version and a per-segment content hash.
package provenance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/fashionweek/mediad/internal/cache"
	"github.com/fashionweek/mediad/internal/clock"
	"github.com/fashionweek/mediad/internal/media"
)

// SourceSpan is the shared-timeline interval covered by one camera.
type SourceSpan struct {
	StartSec  string `json:"start_sec"`
	EndSec    string `json:"end_sec"`
	Creation  string `json:"creation_time"`
	OffsetSec string `json:"origin_offset_sec"`
}

// CameraReceipt captures one camera's provenance even when it is rejected.
type CameraReceipt struct {
	Name      string            `json:"name"`
	Path      string            `json:"path"`
	InputHash string            `json:"input_sha256"`
	Accepted  bool              `json:"accepted"`
	Span      *SourceSpan       `json:"span,omitempty"`
	Error     *media.Error      `json:"error,omitempty"`
	Segments  []SegmentReceipt  `json:"segments,omitempty"`
}

// SegmentReceipt hashes one published media segment.
type SegmentReceipt struct {
	Variant string `json:"variant"`
	Seq     int    `json:"seq"`
	URI     string `json:"uri"`
	SHA256  string `json:"sha256"`
	Bytes   int64  `json:"bytes"`
}

// Receipt is the full per-job provenance document.
type Receipt struct {
	JobID       string                     `json:"job_id"`
	GeneratedAt string                     `json:"generated_at"`
	ToolVersion string                     `json:"ffmpeg_version"`
	ConfigHash  string                     `json:"config_sha256"`
	Cameras     map[string]*CameraReceipt  `json:"cameras"`
	Rejected    []Rejection                `json:"rejected"`
}

// Rejection keeps a structured error in the delivery report.
type Rejection struct {
	Camera    string       `json:"camera"`
	Kind      media.Kind   `json:"kind"`
	Phase     media.Phase  `json:"phase"`
	Path      string       `json:"path"`
	Offset    int64        `json:"byte_offset"`
	Detail    string       `json:"detail"`
	MediaTime string       `json:"media_time,omitempty"`
}

// Builder accumulates the receipt while the pipeline runs.
type Builder struct {
	r *Receipt
}

// NewBuilder starts a receipt for a job and tool version.
func NewBuilder(jobID, toolVersion string, configHash string) *Builder {
	return &Builder{r: &Receipt{
		JobID: jobID, GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
		ToolVersion: toolVersion, ConfigHash: configHash,
		Cameras: map[string]*CameraReceipt{},
	}}
}

// RecordCamera registers an input with its original source digest and span.
func (b *Builder) RecordCamera(name, path, inputHash string, accepted bool,
	span *SourceSpan) *CameraReceipt {
	c := &CameraReceipt{
		Name: name, Path: path, InputHash: inputHash,
		Accepted: accepted, Span: span,
	}
	b.r.Cameras[name] = c
	return c
}

// AddSegment appends a hash for one produced segment file.
func (b *Builder) AddSegment(camera, variant string, seq int, uri string) error {
	c, ok := b.r.Cameras[camera]
	if !ok {
		return media.New(media.PhasePackage, media.KindBounds, "", -1,
			"provenance: unknown camera "+camera)
	}
	hash, err := cache.HashFile(uri)
	if err != nil {
		return err
	}
	st, err := os.Stat(uri)
	if err != nil {
		return media.New(media.PhasePackage, media.KindTool, uri, -1,
			"cannot stat segment for receipt").Wrap(err)
	}
	c.Segments = append(c.Segments, SegmentReceipt{
		Variant: variant, Seq: seq, URI: filepath.Base(uri),
		SHA256: hash, Bytes: st.Size(),
	})
	return nil
}

// Reject preserves a structured rejection in the delivery report.
func (b *Builder) Reject(camera string, e *media.Error) {
	rej := Rejection{
		Camera: camera, Kind: e.Kind, Phase: e.Phase,
		Path: e.Path, Offset: e.Offset, Detail: e.Detail,
	}
	if e.HasTime {
		rej.MediaTime = e.MediaAt.String()
	}
	b.r.Rejected = append(b.r.Rejected, rej)
	if c, ok := b.r.Cameras[camera]; ok {
		c.Accepted = false
		c.Error = e
	}
}

// SpanFromRat builds a SourceSpan from rational bounds and creation time.
func SpanFromRat(creation time.Time, offset, start, end clock.Rat) *SourceSpan {
	return &SourceSpan{
		StartSec:  start.String(),
		EndSec:    end.String(),
		Creation:  creation.UTC().Format(time.RFC3339Nano),
		OffsetSec: offset.String(),
	}
}

// Write atomically persists the receipt as indented JSON.
func (b *Builder) Write(path string) error {
	data, err := json.MarshalIndent(b.r, "", "  ")
	if err != nil {
		return media.New(media.PhasePackage, media.KindTool, path, -1,
			"cannot marshal receipt").Wrap(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return media.New(media.PhasePackage, media.KindTool, path, -1,
			"cannot create receipt directory").Wrap(err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".rcpt-*")
	if err != nil {
		return media.New(media.PhasePackage, media.KindTool, path, -1,
			"cannot create receipt temp").Wrap(err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return media.New(media.PhasePackage, media.KindTool, path, -1,
			"receipt write failed").Wrap(err)
	}
	tmp.Close()
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return media.New(media.PhasePackage, media.KindTool, path, -1,
			"receipt atomic rename failed").Wrap(err)
	}
	return nil
}

// Receipt returns the accumulated document.
func (b *Builder) Receipt() *Receipt { return b.r }
