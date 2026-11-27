package main

import (
	"strconv"
	"strings"
)

// flags is a minimal `--key value` parser sufficient for the fixed relayctl
// command surface; boolean-style needs are expressed via required values.
type flags struct {
	values map[string]string
	order  []string
}

func parseFlags(args []string) (*flags, error) {
	f := &flags{values: map[string]string{}}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			return nil, errf("unexpected argument %q", arg)
		}
		key := strings.TrimPrefix(arg, "--")
		if i+1 >= len(args) {
			return nil, errf("flag --%s requires a value", key)
		}
		if _, exists := f.values[key]; exists {
			return nil, errf("duplicate flag --%s", key)
		}
		f.values[key] = args[i+1]
		f.order = append(f.order, key)
		i++
	}
	return f, nil
}

func (f *flags) get(key string) (string, bool) {
	v, ok := f.values[key]
	return v, ok
}

func (f *flags) required(key string) (string, int) {
	v, ok := f.values[key]
	if !ok || v == "" {
		return "", exitUsage
	}
	return v, exitOK
}

func (f *flags) intOr(key string, def int) (int, int) {
	raw, ok := f.values[key]
	if !ok {
		return def, exitOK
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, exitUsage
	}
	return n, exitOK
}

// unknown reports whether any flag is outside the allowed set.
func (f *flags) unknown(allowed ...string) string {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	for _, k := range f.order {
		if !ok[k] {
			return k
		}
	}
	return ""
}
