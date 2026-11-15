// Package ladder parses and validates the --ladder rung specification and
// groups rungs into video/audio adaptation sets.
package ladder

import (
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"dashkit/internal/model"
)

// Spec is one ladder rung from the command line.
type Spec struct {
	Name      string
	Kind      string // video | audio
	Bandwidth int64
	Width     int
	Height    int
	FrameRate string
	Codecs    string
}

var heights = map[string]int{"240p": 240, "360p": 360, "480p": 480, "720p": 720, "1080p": 1080}

// Parse parses "240p:300k,360p:800k,...;aac:64k,128k" (video;audio).
func Parse(s string) ([]Spec, error) {
	parts := strings.Split(s, ";")
	if len(parts) != 2 {
		return nil, fmt.Errorf("ladder must be \"v:bw,...;aac:bw,...\"")
	}
	var specs []Spec
	for i, v := range strings.Split(parts[0], ",") {
		kv := strings.Split(strings.TrimSpace(v), ":")
		if len(kv) != 2 {
			return nil, fmt.Errorf("bad video rung %q", v)
		}
		h, ok := heights[kv[0]]
		if !ok {
			return nil, fmt.Errorf("unknown video profile %q", kv[0])
		}
		bw, err := parseBandwidth(kv[1])
		if err != nil {
			return nil, err
		}
		w := h * 16 / 9
		if w%2 != 0 {
			w++ // round up to even, e.g. 480p -> 854
		}
		if h == 240 {
			w = 426 // 240p keeps the classic 426x240 geometry
		}
		specs = append(specs, Spec{Name: "v" + kv[0], Kind: "video", Bandwidth: bw,
			Width: w, Height: h, FrameRate: "30000/1001", Codecs: "avc1.4D401F"})
		_ = i
	}
	for _, a := range strings.Split(parts[1], ",") {
		kv := strings.Split(strings.TrimSpace(a), ":")
		if len(kv) == 1 {
			kv = []string{"aac", kv[0]} // bare bandwidth inherits aac
		}
		if len(kv) != 2 || kv[0] != "aac" {
			return nil, fmt.Errorf("bad audio rung %q", a)
		}
		bw, err := parseBandwidth(kv[1])
		if err != nil {
			return nil, err
		}
		specs = append(specs, Spec{Name: fmt.Sprintf("a%s", kv[1]), Kind: "audio",
			Bandwidth: bw, Codecs: "mp4a.40.2"})
	}
	return specs, nil
}

var bwRe = regexp.MustCompile(`^(\d+)([kKmM]?)$`)

func parseBandwidth(s string) (int64, error) {
	m := bwRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("bad bandwidth %q", s)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	switch strings.ToLower(m[2]) {
	case "k":
		n *= 1_000
	case "m":
		n *= 1_000_000
	}
	return n, nil
}

// Validate checks rational frame rate, even geometry, positive bandwidth
// and codec patterns, returning model problems.
func Validate(specs []Spec) []model.Problem {
	var probs []model.Problem
	for _, sp := range specs {
		if sp.Bandwidth <= 0 {
			probs = append(probs, model.Problem{Code: model.CodeBadBandwidth, Rep: sp.Name, Msg: "bandwidth must be > 0"})
		}
		switch sp.Kind {
		case "video":
			if sp.Width%2 != 0 || sp.Height%2 != 0 || sp.Width <= 0 || sp.Height <= 0 {
				probs = append(probs, model.Problem{Code: model.CodeBadGeometry, Rep: sp.Name,
					Msg: fmt.Sprintf("geometry %dx%d must be positive and even", sp.Width, sp.Height)})
			}
			r, ok := new(big.Rat).SetString(sp.FrameRate)
			if !ok || r.Sign() <= 0 {
				probs = append(probs, model.Problem{Code: model.CodeBadFrameRate, Rep: sp.Name,
					Msg: fmt.Sprintf("frame rate %q is not a positive rational", sp.FrameRate)})
			}
			if !strings.HasPrefix(sp.Codecs, "avc1.") {
				probs = append(probs, model.Problem{Code: model.CodeBadCodec, Rep: sp.Name, Msg: "video codecs must be avc1.*"})
			}
		case "audio":
			if !strings.HasPrefix(sp.Codecs, "mp4a.") {
				probs = append(probs, model.Problem{Code: model.CodeBadCodec, Rep: sp.Name, Msg: "audio codecs must be mp4a.*"})
			}
		}
	}
	return probs
}

// Group splits specs into one video and one audio adaptation set.
func Group(specs []Spec) (video, audio []Spec) {
	for _, sp := range specs {
		if sp.Kind == "video" {
			video = append(video, sp)
		} else {
			audio = append(audio, sp)
		}
	}
	return video, audio
}
