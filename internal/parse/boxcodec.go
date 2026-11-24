package parse

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"mediarun/internal/model"
)

// Digest hashes the full byte representation. It is used to prove that an
// unknown box survived a parse/write cycle untouched.
func (b RawBox) Digest() [32]byte { return sha256.Sum256(b.Encode()) }

// FullBytes reconstructs the complete box including its header.
func (b RawBox) FullBytes() []byte { return b.Encode() }

// writeRawHeader writes a size/type header in the compact form. A large
// header is used only when required by the recorded header size.
func writeRawHeader(w io.Writer, typ [4]byte, headerSize, size uint64) {
	if headerSize == 16 {
		var buf [16]byte
		binary.BigEndian.PutUint32(buf[0:4], 1)
		copy(buf[4:8], typ[:])
		binary.BigEndian.PutUint64(buf[8:16], size)
		w.Write(buf[:])
		return
	}
	var buf [8]byte
	binary.BigEndian.PutUint32(buf[0:4], uint32(size))
	copy(buf[4:8], typ[:])
	w.Write(buf[:])
}

// Encode serialises a raw box, including the children of containers.
func (b RawBox) Encode() []byte {
	var body bytes.Buffer
	body.Write(b.Payload)
	for _, c := range b.Children {
		body.Write(c.Encode())
	}
	size := b.HeaderSize + uint64(body.Len())
	var out bytes.Buffer
	if b.HeaderSize == 16 {
		var hdr [16]byte
		binary.BigEndian.PutUint32(hdr[0:4], 1)
		copy(hdr[4:8], b.Type[:])
		binary.BigEndian.PutUint64(hdr[8:16], size)
		out.Write(hdr[:])
	} else {
		var hdr [8]byte
		binary.BigEndian.PutUint32(hdr[0:4], uint32(size))
		copy(hdr[4:8], b.Type[:])
		out.Write(hdr[:])
	}
	out.Write(body.Bytes())
	return out.Bytes()
}

// BodyBytes returns the payload plus any encoded children, i.e. everything
// after this box's header.
func (b RawBox) BodyBytes() []byte {
	if len(b.Children) == 0 {
		return b.Payload
	}
	var buf bytes.Buffer
	buf.Write(b.Payload)
	for _, c := range b.Children {
		buf.Write(c.Encode())
	}
	return buf.Bytes()
}

// Bytes returns the exact original bytes for a leaf box and a freshly
// encoded body for containers (whose children may have been re-laid out).
func (b RawBox) Bytes() []byte {
	if containerTypes[b.String()] {
		return b.Encode()
	}
	var out bytes.Buffer
	writeRawHeader(&out, b.Type, b.HeaderSize, b.Size)
	out.Write(b.Payload)
	return out.Bytes()
}

// WriteRaw serialises a raw box back into w. The recorded size must match
// the reconstructed bytes exactly.
func WriteRaw(w io.Writer, b RawBox) error {
	if b.HeaderSize != 8 && b.HeaderSize != 16 {
		return fmt.Errorf("%w: header size %d", model.ErrBadLength, b.HeaderSize)
	}
	enc := b.Encode()
	if uint64(len(enc)) != b.Size {
		return fmt.Errorf("%w: box %q encodes %d bytes, header says %d",
			model.ErrBadLength, b.Type, len(enc), b.Size)
	}
	_, err := w.Write(enc)
	return err
}
