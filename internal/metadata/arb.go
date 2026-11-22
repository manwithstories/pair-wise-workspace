package metadata

import (
	"fmt"
	"strconv"
	"strings"

	"mediarun/internal/mediaerr"
)

// candidate pairs one layer's serialized value with its source.
type candidate struct {
	source Source
	value  string
}

// Resolve merges the competing layers of one track into a View. The winner of
// each field is the strongest present source; when two different non-default
// values collide the loss is recorded as a Conflict decision (the stronger
// source still wins deterministically). An out-of-bounds crop returns
// CROP_OUT_OF_BOUNDS and no View, so the caller cannot commit output.
func Resolve(in Input) (*View, error) {
	view := &View{TrackID: in.TrackID}

	rot, rotDec, err := resolveRotation(in)
	if err != nil {
		return nil, err
	}
	view.Rotation = rot
	view.Decisions = append(view.Decisions, rotDec)

	crop, cropDec, err := resolveCrop(in)
	if err != nil {
		return nil, err
	}
	if crop != nil {
		if err := crop.checkBounds(in.CodedWidth, in.CodedHeight); err != nil {
			return nil, err
		}
		view.Crop = crop
		view.Decisions = append(view.Decisions, cropDec)
	}

	color, colorDec := resolveColor(in)
	view.Color = color
	view.Decisions = append(view.Decisions, colorDec)
	return view, nil
}

// resolveRotation arbitrates rotation across the layers.
func resolveRotation(in Input) (Rotation, Decision, error) {
	cands := collect(in, func(lv *LayerValues) (string, bool) {
		if lv.Rotation == nil {
			return "", false
		}
		return strconv.Itoa(*lv.Rotation), true
	})
	if len(cands) == 0 {
		return defaultRotation, Decision{
			Field: "rotation", Winner: SourceDefault, Value: "0",
		}, nil
	}
	win, losers, conflict := arbitrate("rotation", cands)
	deg, err := strconv.Atoi(win.value)
	if err != nil {
		return Rotation{}, Decision{}, mediaerr.New(mediaerr.CodeInvalidConfig,
			"unparseable rotation "+win.value)
	}
	deg, err = normalizeRotation(deg)
	if err != nil {
		return Rotation{}, Decision{}, err
	}
	return Rotation{Degrees: deg, Source: win.source},
		Decision{Field: "rotation", Winner: win.source, Value: win.value,
			Losers: losers, Conflict: conflict}, nil
}

// resolveCrop arbitrates an optional crop rectangle.
func resolveCrop(in Input) (*Crop, Decision, error) {
	var winner *Crop
	var winnerSrc Source
	pick := func(src Source, lv *LayerValues) {
		if lv != nil && lv.Crop != nil && (winner == nil || src.rank() > winnerSrc.rank()) {
			winner, winnerSrc = lv.Crop, src
		}
	}
	pick(SourceContainer, in.ContainerLayer)
	pick(SourceTrack, in.TrackLayer)
	pick(SourceOverride, in.Override)
	if winner == nil {
		return nil, Decision{}, nil
	}
	c := *winner
	c.Source = winnerSrc
	return &c, Decision{
		Field: "crop", Winner: winnerSrc,
		Value: fmt.Sprintf("(%d,%d %dx%d)", c.X, c.Y, c.Width, c.Height),
	}, nil
}

// resolveColor arbitrates the color description as one triple.
func resolveColor(in Input) (Color, Decision) {
	cands := collect(in, func(lv *LayerValues) (string, bool) {
		if lv.Color == nil {
			return "", false
		}
		c := lv.Color
		return c.Primaries + "/" + c.Transfer + "/" + c.Matrix, true
	})
	if len(cands) == 0 {
		return defaultColor, Decision{Field: "color", Winner: SourceDefault,
			Value: "bt709/bt709/bt709"}
	}
	win, losers, conflict := arbitrate("color", cands)
	parts := strings.Split(win.value, "/")
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	return Color{Primaries: parts[0], Transfer: parts[1], Matrix: parts[2], Source: win.source},
		Decision{Field: "color", Winner: win.source, Value: win.value,
			Losers: losers, Conflict: conflict}
}

// collect gathers present values from the three configurable layers in
// weakest-to-strongest order so arbitrate sees an ordered slice.
func collect(in Input, pick func(*LayerValues) (string, bool)) []candidate {
	var cands []candidate
	for _, pair := range []struct {
		src Source
		lv  *LayerValues
	}{
		{SourceContainer, in.ContainerLayer},
		{SourceTrack, in.TrackLayer},
		{SourceOverride, in.Override},
	} {
		if pair.lv != nil {
			if v, ok := pick(pair.lv); ok {
				cands = append(cands, candidate{pair.src, v})
			}
		}
	}
	return cands
}

// arbitrate selects the strongest candidate, listing every weaker value that
// differed from the winner as a conflict.
func arbitrate(field string, cands []candidate) (winner candidate, losers []string, conflict bool) {
	winner = cands[0]
	for _, c := range cands[1:] {
		if c.source.rank() > winner.source.rank() {
			if c.value != winner.value {
				conflict = true
				losers = append(losers, string(winner.source)+"="+winner.value)
			}
			winner = c
		} else if c.value != winner.value {
			conflict = true
			losers = append(losers, string(c.source)+"="+c.value)
		}
	}
	return winner, losers, conflict
}

// checkBounds verifies the crop fits inside the coded frame and has positive
// extent. A failure is structured CROP_OUT_OF_BOUNDS.
func (c *Crop) checkBounds(codedW, codedH int) error {
	if c.Width <= 0 || c.Height <= 0 {
		return mediaerr.New(mediaerr.CodeCropOutOfBounds, fmt.Sprintf(
			"crop extent %dx%d must be positive", c.Width, c.Height))
	}
	if c.X < 0 || c.Y < 0 || c.X+c.Width > codedW || c.Y+c.Height > codedH {
		return mediaerr.New(mediaerr.CodeCropOutOfBounds, fmt.Sprintf(
			"crop (%d,%d %dx%d) exceeds coded frame %dx%d",
			c.X, c.Y, c.Width, c.Height, codedW, codedH))
	}
	return nil
}
