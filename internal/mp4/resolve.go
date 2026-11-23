package mp4

import (
	"bytes"
	"os"

	gomp4 "github.com/abema/go-mp4"

	"mediasim/internal/clock"
)

// Parse opens path and resolves every track's sample table as far as the
// bytes allow. It only returns an error when the file cannot be read at all.
func Parse(path string) (*Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, clock.NewError(clock.CatContainer, clock.Span{}, clock.RecoveredNone,
			"open "+path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, clock.NewError(clock.CatContainer, clock.Span{}, clock.RecoveredNone, "stat "+path, err)
	}
	info := &Info{Path: path, Size: st.Size(), Indexed: true}
	res := &fileResolver{f: f, info: info}
	if err := walkBoxes(f, 0, info.Size, res.onBox); err != nil {
		return nil, clock.NewError(clock.CatContainer, clock.Span{}, clock.RecoveredNone,
			"walk boxes in "+path, err)
	}
	res.finish()
	return info, nil
}

type trackDraft struct {
	Track
	mdhdTimescale uint32
	stblOff       int64 // byte offset of the stbl payload, for diagnostics
	stblAvail     int64
	stsz          stszData
	stco          stcoData
	stsc          stscData
	stts          sttsData
	stss          stssData
	wantSync      bool
	hasStbl       bool
	hasStsz       bool
	hasStco       bool
	hasStsc       bool
	hasStts       bool
}

type fileResolver struct {
	f      *os.File
	info   *Info
	tracks []*trackDraft
}

func (r *fileResolver) onBox(path []gomp4.BoxType, offs []int64, b rawBox) {
	switch b.typ {
	case gomp4.BoxTypeMoov():
		r.info.MoovPresent = true
	case gomp4.BoxTypeFtyp():
		if data, err := readPayload(r.f, b); err == nil {
			r.info.Synthetic = bytes.Contains(data, []byte("msmp"))
		}
	case gomp4.BoxTypeMdat():
		r.info.Mdat = append(r.info.Mdat, [2]int64{b.payloadOff(), b.payloadOff() + b.payloadAvail()})
	case gomp4.BoxTypeTrak():
		r.tracks = append(r.tracks, &trackDraft{})
	case gomp4.BoxTypeTkhd():
		if td := r.current(); td != nil {
			r.fillTkhd(td, b)
		}
	case gomp4.BoxTypeMdhd():
		if td := r.current(); td != nil {
			r.fillMdhd(td, b)
		}
	case gomp4.BoxTypeHdlr():
		if td := r.current(); td != nil {
			r.fillHdlr(td, b)
		}
	case gomp4.BoxTypeStbl():
		if td := r.current(); td != nil {
			td.hasStbl = true
			td.stblOff = b.payloadOff()
			td.stblAvail = b.payloadAvail()
		}
	case gomp4.BoxTypeStsd():
		if td := r.current(); td != nil {
			r.fillStsd(td, b)
		}
	case gomp4.BoxTypeStsz():
		if td := r.current(); td != nil {
			td.hasStsz = true
			if d, err := decodeStsz(r.f, b); err == nil {
				td.stsz = d
			}
		}
	case gomp4.BoxTypeStco():
		if td := r.current(); td != nil {
			td.hasStco = true
			if d, err := decodeStco(r.f, b); err == nil {
				td.stco = d
			}
		}
	case gomp4.BoxTypeStsc():
		if td := r.current(); td != nil {
			td.hasStsc = true
			if d, err := decodeStsc(r.f, b); err == nil {
				td.stsc = d
			}
		}
	case gomp4.BoxTypeStts():
		if td := r.current(); td != nil {
			td.hasStts = true
			if d, err := decodeStts(r.f, b); err == nil {
				td.stts = d
			}
		}
	case gomp4.BoxTypeStss():
		if td := r.current(); td != nil {
			if d, err := decodeStss(r.f, b); err == nil {
				td.stss = d
			}
			td.wantSync = true
		}
	}
}

func (r *fileResolver) current() *trackDraft {
	if len(r.tracks) == 0 {
		return nil
	}
	return r.tracks[len(r.tracks)-1]
}

func (r *fileResolver) finish() {
	for _, td := range r.tracks {
		if !td.hasStbl {
			continue
		}
		track := r.resolveTrack(td)
		r.info.Tracks = append(r.info.Tracks, track)
	}
}

// resolveTrack is added with the sample resolver; until then headers alone
// determine whether a track can be indexed at all.
func (r *fileResolver) resolveTrack(td *trackDraft) Track {
	if !td.hasStsz && !td.hasStco && !td.hasStsc {
		r.info.Indexed = false
		r.info.Gaps = append(r.info.Gaps, Gap{
			Kind: GapMissingIndex, TrackID: td.ID, Recovery: clock.RecoveredResync,
			Detail: "stsz/stco/stsc absent",
		})
	}
	return td.Track
}
