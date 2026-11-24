// Package demux unifies MP4 and RTP inputs into model.Packet records. Every
// emitted packet keeps its source track, DTS/PTS, duration and payload; source
// time bases and non-zero origins are preserved without mixing units.
package demux

import (
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"mediadeliver/internal/model"
)

// mp4Track holds the expanded sample table of one trak box.
type mp4Track struct {
	id        uint32
	kind      model.TrackKind
	codec     string
	timescale int64
	mediaTime int64 // elst media_time; non-zero track origin

	// raw sample-table entries, expanded by expandTrack
	stts     []mp4.SttsEntry
	stss     []uint32
	sizes    []uint32
	stsc     []mp4.StscEntry
	chunkOff []int64
	hasStss  bool

	// expanded per-sample fields
	dts        []int64
	durations  []int64
	chunks     []chunk
	keySamples map[int]bool
}

type chunk struct {
	sampleIndex int   // first sample index in this chunk
	fileOffset  int64 // absolute byte offset of that sample
}

// mdatLoc locates one media data box for payload reads.
type mdatLoc struct {
	dataOffset int64 // absolute offset of mdat payload
	dataSize   int64
}

// MP4Reader demuxes an ISO-BMFF file into model.Packet records.
type MP4Reader struct {
	sourceID string
	limits   model.Limits
	r        io.ReadSeeker

	tracks []*mp4Track
	cur    *mp4Track
	inStsd bool
	mdats  []mdatLoc
	path   []mp4.BoxType
	flat   []flatRef
	seq    int64
	// lastOffset is the most recently visited box offset, used to anchor the
	// position of any generic box-walk corruption error.
	lastOffset int64
}

// NewMP4Reader constructs a reader; call Probe before ReadPacket.
func NewMP4Reader(sourceID string, r io.ReadSeeker, limits model.Limits) *MP4Reader {
	return &MP4Reader{sourceID: sourceID, r: r, limits: limits}
}

// Probe walks the box tree, expands the sample tables and validates bounds.
func (m *MP4Reader) Probe() ([]model.TrackInfo, error) {
	_, err := mp4.ReadBoxStructure(m.r, func(h *mp4.ReadHandle) (interface{}, error) {
		bi := &h.BoxInfo
		m.lastOffset = int64(bi.Offset)
		m.path = append(m.path, bi.Type)
		defer func() { m.path = m.path[:len(m.path)-1] }()

		// The first child box of stsd is the sample entry; its fourcc is the
		// codec. It is not a fixed struct type, so read it via the path.
		if m.inStsd && len(m.path) > 0 && m.cur != nil && m.cur.codec == "" {
			if parentIs(m.path, mp4.BoxTypeStsd()) && bi.Type != mp4.BoxTypeStsd() {
				m.cur.codec = bi.Type.String()
			}
		}

		switch bi.Type {
		case mp4.BoxTypeTrak():
			m.cur = &mp4Track{keySamples: map[int]bool{}}
			m.tracks = append(m.tracks, m.cur)
		case mp4.BoxTypeTkhd():
			box, _, e := h.ReadPayload()
			if e != nil {
				return nil, m.corrupt(bi, "read tkhd", e)
			}
			if m.cur != nil {
				m.cur.id = box.(*mp4.Tkhd).TrackID
			}
		case mp4.BoxTypeMdhd():
			box, _, e := h.ReadPayload()
			if e != nil {
				return nil, m.corrupt(bi, "read mdhd", e)
			}
			if m.cur != nil {
				m.cur.timescale = int64(box.(*mp4.Mdhd).Timescale)
			}
		case mp4.BoxTypeHdlr():
			box, _, e := h.ReadPayload()
			if e != nil {
				return nil, m.corrupt(bi, "read hdlr", e)
			}
			if m.cur != nil {
				switch string(box.(*mp4.Hdlr).HandlerType[:]) {
				case "vide":
					m.cur.kind = model.KindVideo
				case "soun":
					m.cur.kind = model.KindAudio
				default:
					m.cur.kind = model.KindUnknownTrack
				}
			}
		case mp4.BoxTypeElst():
			box, _, e := h.ReadPayload()
			if e != nil {
				return nil, m.corrupt(bi, "read elst", e)
			}
			elst := box.(*mp4.Elst)
			if m.cur != nil && len(elst.Entries) > 0 {
				if elst.GetVersion() == 1 {
					m.cur.mediaTime = elst.Entries[0].MediaTimeV1
				} else {
					m.cur.mediaTime = int64(elst.Entries[0].MediaTimeV0)
				}
			}
		case mp4.BoxTypeStsd():
			m.inStsd = true
			defer func() { m.inStsd = false }()
		case mp4.BoxTypeStts(), mp4.BoxTypeStss(), mp4.BoxTypeStsz(),
			mp4.BoxTypeStsc(), mp4.BoxTypeStco():
			if e := m.readSampleBox(h, bi); e != nil {
				return nil, e
			}
		case mp4.BoxTypeMdat():
			m.mdats = append(m.mdats, mdatLoc{
				dataOffset: int64(bi.Offset + bi.HeaderSize),
				dataSize:   int64(bi.Size - bi.HeaderSize),
			})
		}

		if bi.IsSupportedType() {
			return h.Expand()
		}
		return nil, nil
	})
	if err != nil {
		if model.KindOf(err) != model.KindUnknown {
			return nil, err
		}
		return nil, model.Wrap(model.KindCorruptMedia, "demux", "probe mp4",
			m.lastOffset, "box tree walk failed", err)
	}

	if len(m.tracks) > m.limits.MaxTracks {
		return nil, model.At(model.KindLimitExceeded, "demux", "probe mp4", -1,
			fmt.Sprintf("track count %d exceeds limit %d", len(m.tracks), m.limits.MaxTracks))
	}
	for _, tr := range m.tracks {
		if e := m.expandTrack(tr); e != nil {
			return nil, e
		}
	}

	infos := make([]model.TrackInfo, 0, len(m.tracks))
	for _, tr := range m.tracks {
		if len(tr.dts) == 0 {
			continue
		}
		infos = append(infos, model.TrackInfo{
			ID: tr.id, Kind: tr.kind, Codec: tr.codec, Timescale: tr.timescale,
			Packets: int64(len(tr.dts)), FirstDTS: tr.dts[0], LastDTS: tr.dts[len(tr.dts)-1],
		})
	}
	return infos, nil
}

// parentIs reports whether the box immediately above the current path (the
// current box's parent) has the given type.
func parentIs(path []mp4.BoxType, parent mp4.BoxType) bool {
	if len(path) < 2 {
		return false
	}
	return path[len(path)-2] == parent
}

func (m *MP4Reader) corrupt(bi *mp4.BoxInfo, op string, cause error) error {
	return model.Wrap(model.KindCorruptMedia, "demux", op, int64(bi.Offset),
		fmt.Sprintf("box %s at offset %d", bi.Type.String(), bi.Offset), cause)
}
