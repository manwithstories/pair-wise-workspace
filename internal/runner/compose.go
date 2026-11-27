package runner

import (
	"image"
	"path/filepath"

	"fieldcut/internal/media"
	"fieldcut/internal/render"
)

// composeFrame builds the canvas for one output index from every track in
// paint order, loading only the frames that index needs.
func (a *analysis) composeFrame(idx int64) (*image.NRGBA, error) {
	layers := make([]render.Layer, 0, len(a.composite))
	for _, name := range a.composite {
		tf := a.tracks[name]
		if seq, ok := tf.idxSeq[idx]; ok {
			e := tf.entries[seq]
			img, err := render.LoadFrame(filepath.Join(a.inDir, e.Path), e.Width, e.Height)
			if err != nil {
				return nil, media.New(media.KindCorrupt, "render.load", name, seq, err)
			}
			layers = append(layers, render.Layer{Track: name, Seq: seq, Primary: img, Mask: tf.node.Mask})
			continue
		}
		if in, ok := tf.idxInterp[idx]; ok {
			// Interp.Before/After are output indices; resolve them to the
			// source sequences of the valid frames used as interpolation
			// endpoints.
			beforeSeq, okB := tf.idxSeq[in.Before]
			afterSeq, okC := tf.idxSeq[in.After]
			if !okB || !okC {
				return nil, media.Newf(media.KindCorrupt, "render.interp", name, in.Seq,
					"interpolation endpoints missing at indices %d,%d", in.Before, in.After)
			}
			before := tf.entries[beforeSeq]
			after := tf.entries[afterSeq]
			p, err := render.LoadFrame(filepath.Join(a.inDir, before.Path), before.Width, before.Height)
			if err != nil {
				return nil, media.New(media.KindCorrupt, "render.load", name, beforeSeq, err)
			}
			q, err := render.LoadFrame(filepath.Join(a.inDir, after.Path), after.Width, after.Height)
			if err != nil {
				return nil, media.New(media.KindCorrupt, "render.load", name, afterSeq, err)
			}
			layers = append(layers, render.Layer{
				Track: name, Seq: in.Seq, Primary: p, Secondary: q, Mix: in.Weight, Mask: tf.node.Mask,
			})
			continue
		}
		layers = append(layers, render.Layer{Track: name, Seq: idx, Mask: tf.node.Mask})
	}
	img, _, err := render.Composite(a.width, a.height, layers)
	if err != nil {
		return nil, err
	}
	return img, nil
}
