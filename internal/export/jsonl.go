package export

import (
	"encoding/json"
	"io"
)

// WriteJSONL writes the manifest as JSON Lines: one summary record, one
// record per track, then per-track keyframe and wrap-event evidence.
// Everything derives from the same Analysis the text/JSON paths use.
func WriteJSONL(w io.Writer, a *Analysis) error {
	enc := json.NewEncoder(w)
	if err := enc.Encode(map[string]interface{}{
		"type":            "summary",
		"path":            a.Report.Path,
		"box_count":       a.Report.BoxCount,
		"track_count":     a.Report.TrackCount,
		"video_track_id":  a.Report.VideoTrackID,
		"movie_timescale": a.Report.MovieTimescale,
	}); err != nil {
		return err
	}
	for _, t := range a.Report.Tracks {
		if err := enc.Encode(struct {
			Type string `json:"type"`
			TrackReport
		}{Type: "track", TrackReport: t}); err != nil {
			return err
		}
	}
	for _, tr := range a.Movie.Tracks {
		tl := a.Timelines[tr.ID]
		for _, k := range tr.Table.Keyframes {
			s := tr.Table.Samples[k]
			pts := tl.MediaOrigin + s.PTS
			if err := enc.Encode(map[string]interface{}{
				"type":         "keyframe",
				"track_id":     tr.ID,
				"sample":       k,
				"file_offset":  s.Offset,
				"sample_size":  s.Size,
				"pts":          pts,
				"pts_seconds":  tl.MediaToSeconds(pts),
			}); err != nil {
				return err
			}
		}
		for _, we := range tl.WrapEvents {
			if err := enc.Encode(map[string]interface{}{
				"type":      "wrap_event",
				"track_id":  tr.ID,
				"sample":    we.Sample,
				"raw":       we.Raw,
				"unwrapped": we.Unwrapped,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// JSON renders the report as a single JSON document.
func (r *Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}
