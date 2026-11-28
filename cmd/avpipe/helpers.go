package main

import (
	"errors"
	"flag"
	"io"
	"strconv"

	"avpipe/internal/demux"
	"avpipe/internal/media"
)

// readPackets demuxes a file fully into memory. It is only used by the
// selfcheck on small fixtures; the pipeline itself streams.
func readPackets(path string) ([]media.Packet, error) {
	in, err := demux.Open(path, demux.Options{})
	if err != nil {
		return nil, err
	}
	defer in.Close()
	var out []media.Packet
	for {
		p, err := in.Packet()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
}

// newSelfcheckFlagSet builds the flag set for the selfcheck command.
func newSelfcheckFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("selfcheck", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("dir", "", "work directory")
	fs.Int64("seed", 17, "deterministic seed")
	fs.Int("workers", 4, "worker count")
	fs.Int("queue", 8, "queue size")
	fs.Int("cache", 0, "reorder cache size")
	fs.String("segment", "2s", "segment duration")
	fs.Int("wrap-bits", 33, "timestamp wrap width")
	fs.Bool("json", false, "emit JSON")
	return fs
}

// flagInt reads an int flag's parsed value.
func flagInt(fs *flag.FlagSet, name string) int {
	v, err := strconv.Atoi(fs.Lookup(name).Value.String())
	if err != nil {
		return 0
	}
	return v
}

// flagInt64 reads an int64 flag's parsed value.
func flagInt64(fs *flag.FlagSet, name string) int64 {
	v, err := strconv.ParseInt(fs.Lookup(name).Value.String(), 10, 64)
	if err != nil {
		return 0
	}
	return v
}
