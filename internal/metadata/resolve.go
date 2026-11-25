// Package metadata resolves conflicting track descriptions. Values from the
// caller policy win over the bitstream sample description, which wins over the
// container description. Every rejected value is retained for diagnostics.
package metadata

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/model"
)

// Field names used in claims and conflict records.
const (
	FieldRotation  = "rotation"
	FieldCrop      = "crop"
	FieldPrimaries = "primaries"
	FieldTransfer  = "transfer"
	FieldMatrix    = "matrix"
	FieldRange     = "range"
	FieldColor     = "color"
	FieldWidth     = "width"
	FieldHeight    = "height"
	FieldAperture  = "aperture"
)

// fieldOrder fixes evaluation order so output is reproducible.
var fieldOrder = []string{
	FieldRotation, FieldCrop, FieldWidth, FieldHeight, FieldAperture,
	FieldPrimaries, FieldTransfer, FieldMatrix, FieldRange, FieldColor,
}

// Conflict records one resolved field with the winning and losing values.
type Conflict struct {
	Field   string
	Chosen  model.Claim
	Losers  []model.Claim
	TrackID string
}

// Result is the outcome of resolving one track.
type Result struct {
	Description model.Description
	Sources     map[string]model.Source
	Conflicts   []Conflict
	Faults      *fault.List
	Isolated    bool
}

// Options carries the caller policy. A policy value overrides every source.
type Options struct {
	Policy map[string]string
}

// Resolve decides every description field for a track. The track is mutated
// with the resolved description and source table; an unknown colour marker
// isolates the track and records a diagnostic fault.
func Resolve(trak *model.Track, opts Options) Result {
	res := Result{
		Sources: map[string]model.Source{},
		Faults:  &fault.List{},
	}
	trak.Claims.Policy = map[string]string{}
	for k, v := range opts.Policy {
		trak.Claims.Policy[k] = v
	}
	desc := model.Description{}
	cropFromClaim := false
	for _, field := range fieldOrder {
		chosen, losers, invalid, err := choose(trak, field)
		for _, bad := range invalid {
			res.Faults.Add(fault.New(trak.Number, 0, fault.KindMetadata, true,
				"track %s: malformed %s claim %q from %s ignored",
				trak.Label(), field, bad.Value, bad.Source))
		}
		if err != nil {
			res.Faults.Add(err)
			continue
		}
		if chosen.Value == "" {
			continue
		}
		if len(losers) > 0 {
			res.Conflicts = append(res.Conflicts, Conflict{
				Field: field, Chosen: chosen, Losers: losers, TrackID: trak.Label(),
			})
		}
		if field == FieldAperture && cropFromClaim {
			// An explicit crop claim outranks a derived aperture crop.
			continue
		}
		res.Sources[field] = chosen.Source
		if ferr := apply(&desc, field, chosen.Value, trak.Label()); ferr != nil {
			res.Faults.Add(ferr)
			delete(res.Sources, field)
			continue
		}
		if field == FieldCrop {
			cropFromClaim = true
		}
	}
	if err := validateCrop(&desc, trak.Label()); err != nil {
		res.Faults.Add(err)
		delete(res.Sources, FieldCrop)
		desc.Crop = model.Rect{}
	}
	res.Description = desc
	trak.Resolved = desc
	trak.DescSource = res.Sources

	if unknownColor(res.Faults) {
		res.Isolated = true
		trak.Isolated = true
		res.Faults.Add(fault.New(trak.Number, 0, fault.KindMetadata, true,
			"track %s has an unrecognised colour marker; isolated from composition", trak.Label()))
	}
	if desc.Rotation != 0 && desc.Rotation%90 != 0 {
		res.Faults.Add(fault.New(trak.Number, 0, fault.KindMetadata, false,
			"track %s resolved rotation %d is not a quarter turn", trak.Label(), desc.Rotation))
	}
	if desc.Width <= 0 || desc.Height <= 0 {
		res.Faults.Add(fault.New(trak.Number, 0, fault.KindMetadata, true,
			"track %s has no usable dimensions", trak.Label()))
	}
	return res
}

// choose returns the highest-precedence valid claim, the claims it overrides
// and the malformed claims that were skipped.
func choose(trak *model.Track, field string) (model.Claim, []model.Claim, []model.Claim, *fault.Error) {
	layers := []model.Source{model.SourcePolicy, model.SourceBitstream, model.SourceContainer}
	var chosen model.Claim
	var losers, invalid []model.Claim
	for _, src := range layers {
		v, ok := trak.Claims.Get(src, field)
		if !ok || strings.TrimSpace(v) == "" {
			continue
		}
		c := model.Claim{Value: strings.TrimSpace(v), Source: src}
		if _, err := parseField(field, c.Value); err != nil {
			invalid = append(invalid, c)
			continue
		}
		if chosen.Value == "" {
			chosen = c
			continue
		}
		if c.Value != chosen.Value {
			losers = append(losers, c)
		}
	}
	if chosen.Value == "" && len(invalid) > 0 {
		return model.Claim{}, nil, invalid, fault.New(trak.Number, 0, fault.KindMetadata, true,
			"all %s claims for track %s are malformed", field, trak.Label())
	}
	return chosen, losers, invalid, nil
}

// parsedField is the typed value of a claim.
type parsedField struct {
	rotation int
	rect     model.Rect
	number   uint16
	flag     bool
	text     string
	size     int
}

func parseField(field, value string) (parsedField, error) {
	switch field {
	case FieldRotation:
		if value == "unknown" {
			return parsedField{text: value}, nil
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			return parsedField{}, fmt.Errorf("rotation %q: %w", value, err)
		}
		if n%90 != 0 {
			return parsedField{}, fmt.Errorf("rotation %q is not a quarter turn", value)
		}
		n %= 360
		if n < 0 {
			n += 360
		}
		return parsedField{rotation: n}, nil
	case FieldCrop:
		r, err := model.ParseRect(value)
		return parsedField{rect: r}, err
	case FieldWidth, FieldHeight:
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return parsedField{}, fmt.Errorf("%s %q must be positive", field, value)
		}
		return parsedField{size: n}, nil
	case FieldAperture:
		parts := strings.SplitN(value, "x", 2)
		if len(parts) != 2 {
			return parsedField{}, fmt.Errorf("aperture %q must be WxH", value)
		}
		w, errW := strconv.Atoi(parts[0])
		h, errH := strconv.Atoi(parts[1])
		if errW != nil || errH != nil || w <= 0 || h <= 0 {
			return parsedField{}, fmt.Errorf("aperture %q must be positive WxH", value)
		}
		return parsedField{size: w<<16 | h}, nil
	case FieldPrimaries, FieldTransfer, FieldMatrix:
		var n uint16
		if _, err := fmt.Sscanf(value, "%d", &n); err != nil {
			return parsedField{}, fmt.Errorf("%s %q: %w", field, value, err)
		}
		return parsedField{number: n}, nil
	case FieldRange:
		if value != "full" && value != "limited" {
			return parsedField{}, fmt.Errorf("range %q must be full or limited", value)
		}
		return parsedField{flag: value == "full"}, nil
	case FieldColor:
		return parsedField{text: value}, nil
	}
	return parsedField{}, fmt.Errorf("unknown field %q", field)
}

func apply(desc *model.Description, field, value, label string) *fault.Error {
	p, err := parseField(field, value)
	if err != nil {
		return fault.New(-1, 0, fault.KindMetadata, true, "track %s: %v", label, err)
	}
	switch field {
	case FieldRotation:
		if p.text == "unknown" {
			return fault.New(-1, 0, fault.KindMetadata, true,
				"track %s has an unreadable rotation matrix", label)
		}
		desc.Rotation = p.rotation
	case FieldCrop:
		desc.Crop = p.rect
	case FieldWidth:
		desc.Width = p.size
	case FieldHeight:
		desc.Height = p.size
	case FieldAperture:
		w, h := p.size>>16, p.size&0xffff
		// The aperture crop is derived once width and height are known.
		desc.Crop = model.Rect{}
		desc.Crop = apertureCrop(desc.Width, desc.Height, w, h)
	case FieldPrimaries:
		desc.Primaries = p.number
		desc.ColorKnown = true
	case FieldTransfer:
		desc.Transfer = p.number
		desc.ColorKnown = true
	case FieldMatrix:
		desc.Matrix = p.number
		desc.ColorKnown = true
	case FieldRange:
		desc.FullRange = p.flag
	case FieldColor:
		if strings.HasPrefix(p.text, "unknown") {
			return fault.New(-1, 0, fault.KindMetadata, true,
				"track %s carries unknown colour marker %q", label, p.text)
		}
		desc.ColorKnown = true
	}
	return nil
}

// apertureCrop converts a clean aperture into centred crop insets.
func apertureCrop(w, h, aw, ah int) model.Rect {
	if w <= 0 || h <= 0 || aw <= 0 || ah <= 0 || aw > w || ah > h {
		return model.Rect{}
	}
	lr := (w - aw) / 2
	tb := (h - ah) / 2
	return model.Rect{Left: lr, Top: tb, Right: w - aw - lr, Bottom: h - ah - tb}
}

// unknownColor reports whether the fault list carries an unknown colour
// marker diagnostic.
func unknownColor(l *fault.List) bool {
	for _, it := range l.Items {
		if strings.Contains(it.Detail, "unknown colour marker") {
			return true
		}
	}
	return false
}

// ResolveAll resolves every track and returns the combined faults and
// conflicts in deterministic order.
func ResolveAll(tracks []*model.Track, opts Options) (*fault.List, []Conflict) {
	all := &fault.List{}
	var conflicts []Conflict
	for _, t := range tracks {
		r := Resolve(t, opts)
		for _, it := range r.Faults.Items {
			all.Add(it)
		}
		conflicts = append(conflicts, r.Conflicts...)
	}
	sort.SliceStable(conflicts, func(i, j int) bool {
		if conflicts[i].TrackID != conflicts[j].TrackID {
			return conflicts[i].TrackID < conflicts[j].TrackID
		}
		return conflicts[i].Field < conflicts[j].Field
	})
	return all, conflicts
}

// validateCrop rejects crops that would leave no pixels.
func validateCrop(desc *model.Description, label string) *fault.Error {
	if desc.Crop.Empty() {
		return nil
	}
	if desc.Width <= 0 || desc.Height <= 0 {
		return fault.New(-1, 0, fault.KindMetadata, true,
			"track %s has a crop but no dimensions; crop discarded", label)
	}
	r := desc.Crop
	if r.Left+r.Right >= desc.Width || r.Top+r.Bottom >= desc.Height {
		return fault.New(-1, 0, fault.KindMetadata, true,
			"track %s crop %s removes every pixel of %dx%d; crop discarded",
			label, r, desc.Width, desc.Height)
	}
	return nil
}
