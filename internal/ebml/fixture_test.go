package ebml

import (
	"io"
	"strconv"
	"testing"
)

// testTrack describes a track for the in-package fixture builder.
type testTrack struct {
	Number  uint64
	Kind    TrackKind
	Codec   string
	Scale   string
	Default uint64
}

// buildOpts selects structural variants of the fixture.
type buildOpts struct {
	segmentUnknown bool
	unknownCluster bool
	truncateAt     int
	noSegment      bool
	clusterTick    uint64
}

func buildMatroska(t *testing.T, tracks []testTrack, blocks [][]byte, opts buildOpts) []byte {
	t.Helper()
	header := &Encoder{}
	header.Master(IDEBML, func(e *Encoder) {
		e.Uint(IDEBMLVersion, 1)
		e.Uint(IDEBMLReadVersion, 1)
		e.Uint(IDEBMLMaxIDLength, 4)
		e.Uint(IDEBMLMaxSizeLength, 8)
		e.String(IDDocType, "matroska")
		e.Uint(IDDocTypeVersion, 4)
	})
	segBody := &Encoder{}
	segBody.Master(IDInfo, func(e *Encoder) {
		e.Uint(IDTimestampScale, 1_000_000)
		e.Float(IDDuration, 4000)
		e.String(IDMuxingApp, "fixture")
		e.String(IDWritingApp, "fixture")
	})
	segBody.Master(IDTracks, func(e *Encoder) {
		for _, tr := range tracks {
			tr := tr
			e.Master(IDTrackEntry, func(e *Encoder) {
				e.Uint(IDTrackNumber, tr.Number)
				e.Uint(IDTrackUID, tr.Number*100+1)
				e.Uint(IDTrackType, uint64(tr.Kind))
				e.String(IDCodecID, tr.Codec)
				if tr.Scale != "" {
					f, err := strconv.ParseFloat(tr.Scale, 64)
					if err != nil {
						t.Fatalf("bad scale %q", tr.Scale)
					}
					e.Float(IDTrackTimestampScale, f)
				}
				if tr.Default != 0 {
					e.Uint(IDDefaultDuration, tr.Default)
				}
				if tr.Kind == TrackVideo {
					e.Master(IDVideo, func(e *Encoder) {
						e.Uint(IDPixelWidth, 64)
						e.Uint(IDPixelHeight, 36)
					})
				}
				if tr.Kind == TrackAudio {
					e.Master(IDAudio, func(e *Encoder) {
						e.Uint(IDChannels, 1)
						e.Float(IDSamplingFrequency, 48000)
					})
				}
			})
		}
	})
	cluster := func(e *Encoder) {
		e.Uint(IDClusterTimestamp, opts.clusterTick)
		for _, b := range blocks {
			e.Binary(IDSimpleBlock, b)
		}
	}
	// buildMatroska assembles a small Matroska document.
	if opts.unknownCluster {
		segBody.UnknownMaster(IDCluster, 8, cluster)
	} else {
		segBody.Master(IDCluster, cluster)
	}
	out := &Encoder{}
	out.RawBytes(header.Bytes())
	if opts.noSegment {
		return out.Bytes()
	}
	if opts.segmentUnknown {
		out.UnknownHeader(IDSegment, 8)
		out.RawBytes(segBody.Bytes())
	} else {
		out.MasterBytes(IDSegment, segBody.Bytes())
	}
	raw := out.Bytes()
	if opts.truncateAt > 0 && opts.truncateAt < len(raw) {
		raw = raw[:opts.truncateAt]
	}
	return raw
}

type memReaderAt []byte

func (m memReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m)) {
		return 0, io.EOF
	}
	n := copy(p, m[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
