package main

import (
	"strconv"
	"strings"

	"mediaaudit/internal/model"
)

// flags is a minimal --key value parser (no external dependency).
type flags struct {
	vals map[string]string
}

func parseFlags(args []string) (flags, error) {
	f := flags{vals: map[string]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) < 3 || a[:2] != "--" {
			return f, model.NewFault(model.KindInvalid, "", model.FrameRange{}, -1,
				"unexpected token: "+a)
		}
		key := a[2:]
		if i+1 >= len(args) {
			return f, model.NewFault(model.KindInvalid, "", model.FrameRange{}, -1,
				"missing value for --"+key)
		}
		f.vals[key] = args[i+1]
		i++
	}
	return f, nil
}

func (f flags) string(key, def string) string {
	if v, ok := f.vals[key]; ok {
		return v
	}
	return def
}

func (f flags) required(key string) (string, error) {
	v, ok := f.vals[key]
	if !ok || v == "" {
		return "", model.NewFault(model.KindInvalid, "", model.FrameRange{}, -1,
			"missing required --"+key)
	}
	return v, nil
}

func (f flags) int64(key string, def int64) (int64, error) {
	v, ok := f.vals[key]
	if !ok {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, model.NewFault(model.KindInvalid, "", model.FrameRange{}, -1,
			"bad integer for --"+key+": "+v)
	}
	return n, nil
}

func (f flags) uint32(key string, def uint32) (uint32, error) {
	n, err := f.int64(key, int64(def))
	if err != nil {
		return 0, err
	}
	if n < 0 || n > 0xFFFFFFFF {
		return 0, model.NewFault(model.KindInvalid, "", model.FrameRange{}, -1,
			"uint32 out of range for --" + key)
	}
	return uint32(n), nil
}

func (f flags) float64(key string, def float64) (float64, error) {
	v, ok := f.vals[key]
	if !ok {
		return def, nil
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, model.NewFault(model.KindInvalid, "", model.FrameRange{}, -1,
			"bad float for --"+key+": "+v)
	}
	return n, nil
}

// intList parses comma-separated integers (e.g. --vfr 2,3,1).
func (f flags) intList(key string) ([]int, error) {
	v, ok := f.vals[key]
	if !ok || v == "" {
		return nil, nil
	}
	parts := strings.Split(v, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, model.NewFault(model.KindInvalid, "", model.FrameRange{}, -1,
				"bad integer list for --"+key+": "+v)
		}
		out = append(out, n)
	}
	return out, nil
}
