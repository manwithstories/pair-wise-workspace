package container

import (
	"fmt"
	"strings"

	"github.com/abema/go-mp4"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/model"
)

func (p *mp4Parser) parseFtyp(b box, start, end int64) {
	var ftyp mp4.Ftyp
	if err := p.unmarshal(mp4.BoxTypeFtyp(), p.data[start:end], &ftyp); err != nil {
		p.faults.Add(fault.Wrap(-1, b.offset, fault.KindCorrupt, false, err))
		return
	}
	p.file.Brand = strings.TrimRight(string(ftyp.MajorBrand[:]), "\x00 ")
}

func (p *mp4Parser) parseHdlr(b box, start, end int64) model.TrackKind {
	var hdlr mp4.Hdlr
	if err := p.unmarshal(mp4.BoxTypeHdlr(), p.data[start:end], &hdlr); err != nil {
		p.faults.Add(fault.Wrap(-1, b.offset, fault.KindCorrupt, true, err))
		return model.Data
	}
	switch string(hdlr.HandlerType[:]) {
	case "vide":
		return model.Video
	case "soun":
		return model.Audio
	default:
		return model.Data
	}
}

func (p *mp4Parser) parseTkhd(b box, start, end int64, trak *model.Track) {
	var tkhd mp4.Tkhd
	if err := p.unmarshal(mp4.BoxTypeTkhd(), p.data[start:end], &tkhd); err != nil {
		p.faults.Add(fault.Wrap(trak.Number, b.offset, fault.KindCorrupt, false, err))
		return
	}
	trak.ID = tkhd.TrackID
	if w, h := int(tkhd.Width>>16), int(tkhd.Height>>16); w > 0 && h > 0 {
		setClaim(trak, model.SourceContainer, "width", fmt.Sprint(w))
		setClaim(trak, model.SourceContainer, "height", fmt.Sprint(h))
	}
	if rot, ok := matrixRotation(tkhd.Matrix); ok {
		if rot != 0 {
			setClaim(trak, model.SourceContainer, "rotation", fmt.Sprint(rot))
		} else {
			setClaim(trak, model.SourceContainer, "rotation", "0")
		}
	} else {
		setClaim(trak, model.SourceContainer, "rotation", "unknown")
	}
}

// matrixRotation maps a 3x3 fixed-point transform matrix to clockwise degrees.
func matrixRotation(m [9]int32) (int, bool) {
	const one = 1 << 16
	a, b, c, d := m[0], m[1], m[3], m[4]
	switch {
	case a == one && b == 0 && c == 0 && d == one:
		return 0, true
	case a == 0 && b == one && c == -one && d == 0:
		return 90, true
	case a == -one && b == 0 && c == 0 && d == -one:
		return 180, true
	case a == 0 && b == -one && c == one && d == 0:
		return 270, true
	}
	return 0, false
}

func (p *mp4Parser) parseMdhd(b box, start, end int64, trak *model.Track) {
	var mdhd mp4.Mdhd
	if err := p.unmarshal(mp4.BoxTypeMdhd(), p.data[start:end], &mdhd); err != nil {
		p.faults.Add(fault.Wrap(trak.Number, b.offset, fault.KindCorrupt, false, err))
		return
	}
	if mdhd.Timescale == 0 {
		p.faults.Add(fault.New(trak.Number, b.offset, fault.KindCorrupt, false, "mdhd timescale is zero"))
		return
	}
	tb, err := model.NewTimebase(int64(mdhd.Timescale), 1)
	if err != nil {
		p.faults.Add(fault.Wrap(trak.Number, b.offset, fault.KindCorrupt, false, err))
		return
	}
	trak.Timebase = tb
	if mdhd.GetVersion() == 1 {
		trak.Duration = int64(mdhd.DurationV1)
	} else {
		trak.Duration = int64(mdhd.DurationV0)
	}
}

func (p *mp4Parser) parseColr(b box, start, end int64, trak *model.Track) {
	var colr mp4.Colr
	if err := p.unmarshal(mp4.BoxTypeColr(), p.data[start:end], &colr); err != nil {
		p.faults.Add(fault.Wrap(trak.Number, b.offset, fault.KindCorrupt, true, err))
		return
	}
	ctype := string(colr.ColourType[:])
	switch ctype {
	case "nclx", "nclc":
		setClaim(trak, model.SourceContainer, "primaries", fmt.Sprint(colr.ColourPrimaries))
		setClaim(trak, model.SourceContainer, "transfer", fmt.Sprint(colr.TransferCharacteristics))
		setClaim(trak, model.SourceContainer, "matrix", fmt.Sprint(colr.MatrixCoefficients))
		if ctype == "nclx" {
			v := "limited"
			if colr.FullRangeFlag {
				v = "full"
			}
			setClaim(trak, model.SourceContainer, "range", v)
		}
	default:
		setClaim(trak, model.SourceContainer, "color", "unknown:"+ctype)
	}
}

func setClaim(trak *model.Track, src model.Source, key, value string) {
	if trak.Claims.Policy == nil {
		trak.Claims.Policy = map[string]string{}
		trak.Claims.Bitstream = map[string]string{}
		trak.Claims.Container = map[string]string{}
	}
	switch src {
	case model.SourcePolicy:
		trak.Claims.Policy[key] = value
	case model.SourceBitstream:
		trak.Claims.Bitstream[key] = value
	case model.SourceContainer:
		trak.Claims.Container[key] = value
	}
}
