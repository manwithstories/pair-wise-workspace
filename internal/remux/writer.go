// Package remux writes the unified media package. It copies coded sample bytes
// verbatim: no decode, no re-encode, no padding. The package is a directory with
// one essence file per output track, a bytes manifest and a hash manifest, so a
// reviewer can prove pre- and post-remux samples are identical.
package remux

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mediasync/internal/model"
)

// maxFileBytes caps any single essence file at the 64 MiB block budget.
const maxFileBytes = 64 << 20

// TrackInput is one mapped track to write.
type TrackInput struct {
	// Name is the output file stem, e.g. "program-video".
	Name string
	// Track carries the coded samples; Sample.Offset refers to Reader.
	Track *model.Track
	// Reader supplies coded bytes at Sample.Offset.
	Reader io.ReaderAt
}

// TrackOutput reports what was written for one track.
type TrackOutput struct {
	Name     string          `json:"name"`
	TrackID  uint32          `json:"track_id"`
	Kind     model.TrackKind `json:"kind"`
	Codec    string          `json:"codec"`
	Samples  int             `json:"samples"`
	Bytes    int64           `json:"bytes"`
	SHA256   string          `json:"sha256"`
	Rotation int             `json:"rotation"`
	SourceW  int             `json:"source_width"`
	SourceH  int             `json:"source_height"`
	DisplayW int             `json:"display_width"`
	DisplayH int             `json:"display_height"`
	Crop     model.Crop      `json:"crop"`
	Colour   *model.Colour   `json:"colour,omitempty"`
}

// Result is the manifest of one remux run.
type Result struct {
	Tracks     []TrackOutput `json:"tracks"`
	Samples    int           `json:"samples"`
	Bytes      int64         `json:"bytes"`
	PackageSHA string        `json:"package_sha256"`
}

// Writer writes a package directory.
type Writer struct {
	dir string
}

// NewWriter prepares dir, rejecting path escapes and refusing to overwrite a
// non-directory.
func NewWriter(dir string) (*Writer, error) {
	if dir == "" {
		return nil, model.NewError(model.ErrParamInvalid, 0, 0, "empty output directory")
	}
	clean := filepath.Clean(dir)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, model.NewError(model.ErrPathEscape, 0, 0, "output directory escapes working tree: %s", dir)
	}
	if st, err := os.Stat(clean); err == nil && !st.IsDir() {
		return nil, model.NewError(model.ErrParamInvalid, 0, 0, "output path exists and is not a directory: %s", dir)
	}
	if err := os.MkdirAll(clean, 0o755); err != nil {
		return nil, err
	}
	return &Writer{dir: clean}, nil
}

// Dir returns the package directory.
func (w *Writer) Dir() string { return w.dir }

// WriteTrack copies one track's samples to Name.essence and verifies the
// per-track hash matches the source reader's bytes.
func (w *Writer) WriteTrack(in TrackInput) (TrackOutput, error) {
	if in.Track == nil {
		return TrackOutput{}, model.NewError(model.ErrParamInvalid, 0, 0, "nil track for %q", in.Name)
	}
	if in.Reader == nil {
		return TrackOutput{}, model.NewError(model.ErrParamInvalid, in.Track.ID, 0, "nil reader for %q", in.Name)
	}
	if err := validStem(in.Name); err != nil {
		return TrackOutput{}, err
	}
	path := filepath.Join(w.dir, in.Name+".essence")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return TrackOutput{}, err
	}
	defer f.Close()

	h := sha256.New()
	var total int64
	buf := make([]byte, 0, 64<<10)
	for i := range in.Track.Samples {
		s := &in.Track.Samples[i]
		if s.Size < 0 {
			return TrackOutput{}, model.NewError(model.ErrSizeLimit, in.Track.ID, s.Offset, "sample %d has negative size", s.Index)
		}
		if s.Size > maxFileBytes || total+int64(s.Size) > maxFileBytes {
			return TrackOutput{}, model.NewError(model.ErrSizeLimit, in.Track.ID, s.Offset,
				"track %q would exceed the %d byte file limit", in.Name, maxFileBytes)
		}
		if buf == nil || cap(buf) < s.Size {
			buf = make([]byte, s.Size)
		}
		chunk := buf[:s.Size]
		if s.Size > 0 {
			n, err := in.Reader.ReadAt(chunk, s.Offset)
			if err != nil && err != io.EOF {
				return TrackOutput{}, model.WrapError(model.ErrTruncatedBox, in.Track.ID, s.Offset, err)
			}
			if n != s.Size {
				return TrackOutput{}, model.NewError(model.ErrTruncatedBox, in.Track.ID, s.Offset,
					"short read for sample %d: got %d of %d", s.Index, n, s.Size)
			}
		}
		if _, err := f.Write(chunk); err != nil {
			return TrackOutput{}, err
		}
		if _, err := h.Write(chunk); err != nil {
			return TrackOutput{}, err
		}
		total += int64(s.Size)
	}
	if err := f.Sync(); err != nil {
		return TrackOutput{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))

	meta := in.Track.Meta
	dw, dh := meta.SourceWidth, meta.SourceHeight
	if in.Track.Width > 0 {
		dw, dh = displayDims(meta, in.Track.Width, in.Track.Height)
	}
	return TrackOutput{
		Name: in.Name, TrackID: in.Track.ID, Kind: in.Track.Kind, Codec: in.Track.Codec,
		Samples: len(in.Track.Samples), Bytes: total, SHA256: sum,
		Rotation: meta.Rotation, SourceW: meta.SourceWidth, SourceH: meta.SourceHeight,
		DisplayW: dw, DisplayH: dh, Crop: meta.Crop, Colour: meta.Colour,
	}, nil
}

// displayDims applies crop then rotation to the coded frame.
func displayDims(meta model.DisplayMeta, w, h int) (int, int) {
	w -= meta.Crop.Left + meta.Crop.Right
	h -= meta.Crop.Top + meta.Crop.Bottom
	if meta.Rotation == 90 || meta.Rotation == 270 {
		w, h = h, w
	}
	return w, h
}

// validStem rejects names that could leave the package directory.
func validStem(name string) error {
	if name == "" {
		return model.NewError(model.ErrParamInvalid, 0, 0, "empty track name")
	}
	if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return model.NewError(model.ErrPathEscape, 0, 0, "track name %q is not a plain file stem", name)
	}
	return nil
}

// WriteManifest serialises the package manifest atomically.
func (w *Writer) WriteManifest(res Result) (string, error) {
	// The package hash is the hash of the per-track hashes in output order, so
	// it is independent of file timestamps and paths.
	ph := sha256.New()
	for _, t := range res.Tracks {
		fmt.Fprintf(ph, "%s:%s:", t.Name, t.SHA256)
	}
	res.PackageSHA = hex.EncodeToString(ph.Sum(nil))
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(w.dir, "package.json")
	if err := writeAtomic(path, data); err != nil {
		return "", err
	}
	return res.PackageSHA, nil
}

// Verify re-reads the written essence files and checks their hashes against the
// manifest, which is the proof that remuxing did not change samples.
func (w *Writer) Verify(res Result) error {
	for _, t := range res.Tracks {
		path := filepath.Join(w.dir, t.Name+".essence")
		f, err := os.Open(path)
		if err != nil {
			return model.NewError(model.ErrUnrecoverable, t.TrackID, 0, "verify: %v", err)
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil {
			return model.NewError(model.ErrUnrecoverable, t.TrackID, 0, "verify %q: %v", t.Name, err)
		}
		if hex.EncodeToString(h.Sum(nil)) != t.SHA256 {
			return model.NewError(model.ErrUnrecoverable, t.TrackID, 0, "verify %q: hash mismatch after write", t.Name)
		}
		if n != t.Bytes {
			return model.NewError(model.ErrUnrecoverable, t.TrackID, 0, "verify %q: %d bytes, manifest says %d", t.Name, n, t.Bytes)
		}
	}
	return nil
}

// writeAtomic writes path through a temp file in the same directory.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
