package main

import (
	"bytes"
	"io"

	"mediamux/ingest"
	"mediamux/media"
	"mediamux/testdata"
	"mediamux/transport"
)

// fixtureReport 是损坏夹具的恢复报告 JSON 行。
type fixtureReport struct {
	Fixture     string `json:"fixture"`
	Kind        string `json:"kind"`
	Valid       int    `json:"valid"`
	Invalid     int    `json:"invalid"`
	ErrorOffset int64  `json:"error_offset"` // 首个错误的原始字节偏移
	Recovered   int    `json:"recovered"`    // 首个错误之后仍解析出的有效记录数
}

// runFixtures 逐个重放故障夹具，报告错误偏移与恢复计数。
func runFixtures() []fixtureReport {
	var out []fixtureReport
	for _, fx := range testdata.Fixtures() {
		rep := fixtureReport{Fixture: fx.Name, Kind: fx.Kind, ErrorOffset: -1}
		seenErr := false
		record := func(err error) {
			if err != nil {
				rep.Invalid++
				if !seenErr {
					rep.ErrorOffset = media.As(err).Offset
					seenErr = true
				}
				return
			}
			rep.Valid++
			if seenErr {
				rep.Recovered++
			}
		}
		switch fx.Kind {
		case "srt", "vtt":
			var next func() (*ingest.Event, error)
			if fx.Kind == "srt" {
				p := ingest.NewSRTParser(bytes.NewReader(fx.Data), media.FrameRate25, testdata.MaxRecord)
				next = p.Next
			} else {
				p := ingest.NewVTTParser(bytes.NewReader(fx.Data), media.FrameRate25, testdata.MaxRecord)
				next = p.Next
			}
			for {
				_, err := next()
				if err == io.EOF {
					break
				}
				record(err)
			}
		case "rtp":
			rd := transport.NewReader(bytes.NewReader(fx.Data), testdata.MaxPacket)
			for {
				_, err := rd.Next()
				if err == io.EOF {
					break
				}
				record(err)
				if err != nil && !media.IsRecoverable(err) {
					break // 截断尾部：流终止，不可恢复
				}
			}
		}
		out = append(out, rep)
	}
	return out
}
