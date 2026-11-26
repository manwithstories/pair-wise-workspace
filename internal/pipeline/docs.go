package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"pairflow/internal/media"
	"pairflow/internal/store"
	"pairflow/internal/timeline"
	"pairflow/internal/transport"
	"pairflow/internal/video"
)

// transportWire is the committed transport record: the report plus the raw
// delivery log the verifier replays.
type transportWire struct {
	Report transport.Report      `json:"report"`
	Wire   []transport.WireEvent `json:"wire"`
}

// colorSource records one segment's normalized color provenance.
type colorSource struct {
	Track   string `json:"track"`
	Clip    string `json:"clip"`
	Source  string `json:"source_space"`
	Pattern string `json:"pattern"`
}

// colorDocument is the color stage artifact.
type colorDocument struct {
	Destination string        `json:"destination"`
	Sources     []colorSource `json:"sources"`
	JSON        []byte        `json:"-"`
}

// prepareColor validates every video segment's color description before the
// color stage commits, so an unknown space fails with a locatable error.
func prepareColor(res []timeline.Resolved) *media.Error {
	for _, tr := range res {
		if tr.Kind != "video" {
			continue
		}
		for _, seg := range tr.Segments {
			if seg.Video == nil {
				continue
			}
			if _, err := video.ParseSpace(seg.Video.Space, tr.TrackID+"/"+seg.ClipID, "video.space"); err != nil {
				return err
			}
		}
	}
	return nil
}

// colorDoc builds the color provenance document.
func colorDoc(res []timeline.Resolved) (*colorDocument, *media.Error) {
	doc := &colorDocument{Destination: "srgb"}
	for _, tr := range res {
		if tr.Kind != "video" {
			continue
		}
		for _, seg := range tr.Segments {
			if seg.Video == nil {
				continue
			}
			sp, err := video.ParseSpace(seg.Video.Space, tr.TrackID+"/"+seg.ClipID, "video.space")
			if err != nil {
				return nil, err
			}
			doc.Sources = append(doc.Sources, colorSource{
				Track: tr.TrackID, Clip: seg.ClipID, Source: sp.Name, Pattern: seg.Video.Pattern,
			})
		}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, media.Wrap(err, media.ClassInternal, "pipeline.color_encode", "cannot encode color document")
	}
	doc.JSON = data
	return doc, nil
}

// clearGenerated removes artifacts from a previous generation so a rerun is
// reproducible: committed stages under out/, the manifest and the journal.
func clearGenerated(j *store.Jail) *media.Error {
	for _, rel := range []string{store.OutDir, store.StagingDir, store.Manifest, "wal.jsonl"} {
		abs, err := j.Resolve(rel)
		if err != nil {
			// Absent components are fine; Resolve already refused traversal.
			continue
		}
		if rerr := os.RemoveAll(abs); rerr != nil {
			return media.Wrap(rerr, media.ClassInternal, "pipeline.clear", "cannot clear previous run output").WithPath(rel)
		}
	}
	return store.SyncDir(j.Root())
}

// outPath joins an artifact's path for display.
func outPath(root, rel string) string { return filepath.Join(root, rel) }

// fmtPct formats a percentage to two decimals.
func fmtPct(v float64) string { return fmt.Sprintf("%.2f", v) }
