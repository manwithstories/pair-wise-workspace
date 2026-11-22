package worker

import (
	"sort"

	"mediad/internal/checkpoint"
	segmenthash "mediad/internal/hash"
)

func outputsFromState(st *checkpoint.State) []Output {
	var outs []Output
	for _, c := range st.Chunks {
		for _, rec := range c.Segments {
			outs = append(outs, Output{
				URI: rec.URI, Sequence: rec.Sequence,
				StartTicks: rec.StartTicks, EndTicks: rec.EndTicks,
				InputHash: "", OutputHash: rec.SHA256, OutputSize: rec.Size,
				Determined: rec.Determined, Gap: rec.Gap,
				InputKnown: true, OutputKnown: true, Chunk: rec.Chunk,
			})
		}
	}
	return outs
}

func CheckpointFromResult(res *Result, opts Options, prior *checkpoint.State) (*checkpoint.State, error) {
	st, err := checkpoint.NewState(res.ManifestSHA, opts.ChunkSize)
	if err != nil {
		return nil, err
	}
	if prior != nil {
		if _, err := checkpoint.Resume(prior, res.ManifestSHA); err != nil {
			return nil, err
		}
	}
	chunks := make(map[int]checkpoint.Chunk)
	if prior != nil {
		for _, c := range prior.Chunks {
			chunks[c.Index] = c
		}
	}
	for chunk := 0; chunk < res.Chunks; chunk++ {
		start, end := checkpoint.ChunkRange(chunk, len(res.Outputs), opts.ChunkSize)
		if start >= end {
			continue
		}
		c := checkpoint.Chunk{Index: chunk}
		for _, o := range res.Outputs[start:end] {
			if o.OutputKnown {
				c.Segments = append(c.Segments, segmenthash.Record{
					URI: o.URI, Sequence: o.Sequence, Role: "output", Chunk: chunk,
					Size: o.OutputSize, SHA256: o.OutputHash,
					StartTicks: o.StartTicks, EndTicks: o.EndTicks,
					Gap: o.Gap, Determined: o.Determined,
				})
			}
			c.Faults = append(c.Faults, o.Faults...)
		}
		if len(c.Segments) > 0 || prior == nil {
			chunks[chunk] = c
		}
	}
	indexes := make([]int, 0, len(chunks))
	for k := range chunks {
		indexes = append(indexes, k)
	}
	sort.Ints(indexes)
	for _, k := range indexes {
		if err := st.AddChunk(chunks[k]); err != nil {
			return nil, err
		}
	}
	return st, nil
}
