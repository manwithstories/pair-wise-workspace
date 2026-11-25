package demux

import (
	"bytes"
	"errors"
	"io"

	gomp4 "github.com/abema/go-mp4"
)

// GoMP4TopLevel uses go-mp4's box header reader to enumerate top-level boxes.
// FieldPost keeps its own offset-accurate validator alongside this dependency;
// this helper also gives tests a second parser's header view.
func GoMP4TopLevel(data []byte) ([]gomp4.BoxInfo, error) {
	r := bytes.NewReader(data)
	var out []gomp4.BoxInfo
	for {
		bi, err := gomp4.ReadBoxInfo(r)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, *bi)
		if _, err := bi.SeekToEnd(r); err != nil {
			return nil, err
		}
	}
}
