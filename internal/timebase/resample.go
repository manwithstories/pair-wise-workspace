package timebase

import (
	"fmt"
	"strconv"
	"strings"

	"subtool/internal/subtitle"
)

type Rate struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

func ParseRate(value string) (Rate, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Rate{}, fmt.Errorf("empty frame rate")
	}
	if strings.Contains(value, "/") {
		parts := strings.Split(value, "/")
		if len(parts) != 2 {
			return Rate{}, fmt.Errorf("frame rate must be NUM/DEN")
		}
		num, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || num <= 0 {
			return Rate{}, fmt.Errorf("invalid frame rate numerator")
		}
		den, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || den <= 0 {
			return Rate{}, fmt.Errorf("invalid frame rate denominator")
		}
		return reduce(Rate{Num: num, Den: den}), nil
	}
	if strings.Contains(value, ".") {
		parts := strings.SplitN(value, ".", 2)
		if len(parts[1]) > 6 {
			return Rate{}, fmt.Errorf("too many decimal places")
		}
		whole, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || whole < 0 {
			return Rate{}, fmt.Errorf("invalid frame rate")
		}
		scale := int64(1)
		for range parts[1] {
			scale *= 10
		}
		frac, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return Rate{}, fmt.Errorf("invalid frame rate fraction")
		}
		rate := reduce(Rate{Num: whole*scale + frac, Den: scale})
		if rate.Num <= 0 {
			return Rate{}, fmt.Errorf("frame rate must be positive")
		}
		return rate, nil
	}
	num, err := strconv.ParseInt(value, 10, 64)
	if err != nil || num <= 0 {
		return Rate{}, fmt.Errorf("invalid frame rate")
	}
	return Rate{Num: num, Den: 1}, nil
}

func (r Rate) String() string {
	if r.Den == 1 {
		return strconv.FormatInt(r.Num, 10)
	}
	return fmt.Sprintf("%d/%d", r.Num, r.Den)
}

func (r Rate) Valid() bool {
	return r.Num > 0 && r.Den > 0
}

func FrameForMS(ms int64, rate Rate) int64 {
	return roundDiv(ms*rate.Num, 1000*rate.Den)
}

func MSForFrame(frame int64, rate Rate) int64 {
	return roundDiv(frame*1000*rate.Den, rate.Num)
}

func AlignMS(ms int64, rate Rate) int64 {
	return MSForFrame(FrameForMS(ms, rate), rate)
}

func ScaleMS(ms int64, inRate, outRate Rate) int64 {
	if !inRate.Valid() || !outRate.Valid() {
		return ms
	}
	numerator := ms * outRate.Num * inRate.Den
	denominator := outRate.Den * inRate.Num
	return roundDiv(numerator, denominator)
}

type Mode string

const (
	ModeAlignOnly   Mode = "align_only"
	ModeSpeedFollow Mode = "speed_follow"
)

func ModeFor(speedFollow bool) Mode {
	if speedFollow {
		return ModeSpeedFollow
	}
	return ModeAlignOnly
}

func Resample(track *subtitle.Track, inRate, outRate Rate, speedFollow bool) (*subtitle.Track, error) {
	if track == nil {
		return nil, fmt.Errorf("nil track")
	}
	if !inRate.Valid() || !outRate.Valid() {
		return nil, fmt.Errorf("invalid input or output frame rate")
	}
	result := &subtitle.Track{Format: track.Format, Cues: make([]subtitle.Cue, len(track.Cues))}
	copy(result.Cues, track.Cues)
	for i := range result.Cues {
		cue := &result.Cues[i]
		if speedFollow {
			cue.StartMS = AlignMS(ScaleMS(cue.StartMS, inRate, outRate), outRate)
			cue.EndMS = AlignMS(ScaleMS(cue.EndMS, inRate, outRate), outRate)
		}
	}
	subtitle.Normalize(result)
	return result, nil
}

func roundDiv(numerator, denominator int64) int64 {
	if numerator < 0 {
		return -(((-numerator) + denominator/2) / denominator)
	}
	return (numerator + denominator/2) / denominator
}

func reduce(rate Rate) Rate {
	a, b := rate.Num, rate.Den
	for b != 0 {
		a, b = b, a%b
	}
	return Rate{Num: rate.Num / a, Den: rate.Den / a}
}
