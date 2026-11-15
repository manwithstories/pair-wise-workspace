package main

import (
	"bytes"

	"loudcheck/internal/loudness"
	"loudcheck/internal/wavio"
)

type memoryFile struct {
	buffer []byte
	offset int64
}

func (m *memoryFile) Write(p []byte) (int, error) {
	end := m.offset + int64(len(p))
	if int(end) > len(m.buffer) {
		grown := make([]byte, end)
		copy(grown, m.buffer)
		m.buffer = grown
	}
	copy(m.buffer[m.offset:], p)
	m.offset = end
	return len(p), nil
}

func (m *memoryFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0:
		m.offset = offset
	case 1:
		m.offset += offset
	case 2:
		m.offset = int64(len(m.buffer)) + offset
	}
	return m.offset, nil
}

func (m *memoryFile) Close() error { return nil }

func measurePlanar(header wavio.WriteHeader, planar [][]float64) (loudness.EngineResult, error) {
	file := &memoryFile{}
	writer, err := wavio.NewWriter(file, header)
	if err != nil {
		return loudness.EngineResult{}, err
	}
	if err := writer.WriteFrames(planar); err != nil {
		return loudness.EngineResult{}, err
	}
	if err := writer.Close(); err != nil {
		return loudness.EngineResult{}, err
	}
	result, _, err := loudness.MeasureStream(bytes.NewReader(file.buffer), 8192)
	return result, err
}
