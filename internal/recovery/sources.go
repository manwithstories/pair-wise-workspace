package recovery

import (
	"os"
	"path/filepath"

	"mediad/internal/codec"
	"mediad/internal/fault"
	"mediad/internal/manifest"
)

type loadedSegment struct {
	index    int
	hls      manifest.Segment
	path     string
	data     []byte
	hash     string
	fragment *manifest.ParsedFragment
	nals     []codec.NAL
	pcm      []int16
}

func inputPaths(root string) (dashPath, hlsPath string) {
	input := filepath.Join(root, "input")
	return filepath.Join(input, "dash.mpd"), filepath.Join(input, "hls.m3u8")
}

func loadSources(root string) (*manifest.Manifest, *manifest.Manifest, []*loadedSegment, []string, error) {
	dashPath, hlsPath := inputPaths(root)
	dashData, err := os.ReadFile(dashPath)
	if err != nil {
		return nil, nil, nil, nil, fault.New(fault.IO, dashPath, -1, "read DASH: %v", err)
	}
	dashManifest, err := manifest.ParseDASH(dashPath, dashData)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	hlsData, err := os.ReadFile(hlsPath)
	if err != nil {
		return nil, nil, nil, nil, fault.New(fault.IO, hlsPath, -1, "read HLS: %v", err)
	}
	hlsManifest, err := manifest.ParseHLS(hlsPath, hlsData)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if len(hlsManifest.Segments) == 0 {
		return nil, nil, nil, nil, fault.New(fault.InvalidManifest, hlsPath, 0, "no HLS segments")
	}

	segs := make([]*loadedSegment, 0, len(hlsManifest.Segments))
	hashes := make([]string, 0, len(hlsManifest.Segments))
	for _, s := range hlsManifest.Segments {
		p := manifest.ResolveURI(hlsPath, s.URI)
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, nil, nil, nil, fault.New(fault.IO, p, -1, "read segment: %v", err)
		}
		h := HashBytes(data)
		segs = append(segs, &loadedSegment{index: s.Index, hls: s, path: p, data: data, hash: h})
		hashes = append(hashes, h)
	}
	return dashManifest, hlsManifest, segs, hashes, nil
}

func readAndHash(path string) ([]byte, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fault.New(fault.IO, path, -1, "read file: %v", err)
	}
	return data, HashBytes(data), nil
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fault.New(fault.IO, filepath.Dir(path), -1, "create directory: %v", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fault.New(fault.IO, tmp, -1, "write file: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fault.New(fault.IO, path, -1, "rename file: %v", err)
	}
	return nil
}
