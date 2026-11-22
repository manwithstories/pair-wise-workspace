// Package remux writes a lossless, self-contained fragmented MP4 from
// demuxed tracks. Samples, durations and composition offsets are copied
// verbatim; decode times are wrap-unwrapped so a wrapped source counter
// still yields a monotonic timeline. Output is deterministic: the same
// input and track mapping always produce identical bytes.
package remux
