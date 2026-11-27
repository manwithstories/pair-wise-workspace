package pipeline

import "bcastqc/internal/ebml"

// parseOutput 用独立 EBML 解读器回读再封装字节。
func parseOutput(out []byte) (*ebml.Document, error) {
	return ebml.ParseDocument(ebml.NewBytesReader(out))
}
