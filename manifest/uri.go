package manifest

import (
	"net/url"
	"path"
	"strings"

	"fieldpost/internal/core"
)

// SafeURI ensures a manifest reference stays below its local source directory.
// It intentionally accepts relative local URIs only: FieldPost has no network
// fetcher and must never follow path traversal or an external scheme.
func SafeURI(source, ref string, offset int64) error {
	if ref == "" {
		return core.ManifestError(source, offset, "empty URI")
	}
	if strings.ContainsRune(ref, 0) {
		return core.ManifestError(source, offset, "URI contains NUL byte")
	}
	if strings.Contains(ref, "\\") {
		return core.ManifestError(source, offset, "backslash path separator rejected: %q", ref)
	}
	if strings.HasPrefix(ref, "/") {
		return core.ManifestError(source, offset, "absolute URI escapes source root: %q", ref)
	}
	if strings.Contains(ref, "..") {
		// Reject any parent traversal, including encoded forms after decoding.
		decoded, err := url.PathUnescape(ref)
		if err != nil {
			return core.ManifestError(source, offset, "invalid percent-encoding in URI %q: %v", ref, err)
		}
		if decoded != ref && strings.Contains(decoded, "..") {
			return core.ManifestError(source, offset, "encoded path traversal in URI %q", ref)
		}
		clean := path.Clean(ref)
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return core.ManifestError(source, offset, "path traversal in URI %q", ref)
		}
	}
	if dec, err := url.PathUnescape(ref); err == nil {
		if path.IsAbs(dec) || strings.HasPrefix(dec, "..") || strings.Contains(dec, "/../") {
			return core.ManifestError(source, offset, "out-of-bounds URI after decode: %q", ref)
		}
	}
	u, err := url.Parse(ref)
	if err != nil {
		return core.ManifestError(source, offset, "invalid URI %q: %v", ref, err)
	}
	if u.Scheme != "" || u.Host != "" {
		return core.ManifestError(source, offset, "external URI not permitted: %q", ref)
	}
	return nil
}

func rejectDuplicateNumbers(source string, nums []int64, offsets []int64) error {
	seen := make(map[int64]int64, len(nums))
	for i, n := range nums {
		if prev, ok := seen[n]; ok {
			off := int64(0)
			if i < len(offsets) {
				off = offsets[i]
			}
			return core.ManifestError(source, off, "duplicate segment number %d (first at offset %d)", n, prev)
		}
		var off int64
		if i < len(offsets) {
			off = offsets[i]
		}
		seen[n] = off
	}
	return nil
}
