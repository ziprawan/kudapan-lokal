// Package bsv implements a reader for Cygames Binary Separated Values (BSV)
// manifest files.
package bsv

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
)

const maxVLQBytes = 10

// ReadVLQ decodes a big-endian 7-bit continuation-bit variable-length integer
// from buf starting at *offset, consuming at most maxBytes bytes.
func ReadVLQ(buf []byte, offset *int, maxBytes int) (uint64, error) {
	if offset == nil || *offset < 0 {
		return 0, errors.New("invalid vlq offset")
	}
	if maxBytes <= 0 {
		maxBytes = maxVLQBytes
	}

	var n uint64
	for count := 0; count < maxBytes; count++ {
		if *offset >= len(buf) {
			return 0, fmt.Errorf("vlq truncated at offset %d: %w", *offset, io.ErrUnexpectedEOF)
		}
		b := buf[*offset]
		*offset++

		n |= uint64(b & 0x7f)
		if b&0x80 == 0 {
			return n, nil
		}
		if count == maxBytes-1 {
			return 0, fmt.Errorf("vlq exceeded max %d bytes at offset %d", maxBytes, *offset)
		}
		if n > math.MaxUint64>>7 {
			return 0, fmt.Errorf("vlq overflow at offset %d", *offset)
		}
		n <<= 7
	}
	return n, nil
}

// ReadUNum decodes a fixed-width big-endian unsigned integer of size (1..8)
// bytes from buf starting at *offset.
func ReadUNum(buf []byte, offset *int, size int) (uint64, error) {
	if offset == nil || *offset < 0 {
		return 0, errors.New("invalid unum offset")
	}
	if size < 1 || size > 8 {
		return 0, fmt.Errorf("invalid unum byte width %d: want 1..8", size)
	}
	if size > len(buf)-*offset {
		return 0, fmt.Errorf("unum truncated at offset %d (need %d bytes): %w", *offset, size, io.ErrUnexpectedEOF)
	}

	var n uint64
	for i := 0; i < size; i++ {
		n = (n << 8) | uint64(buf[*offset+i])
	}
	*offset += size
	return n, nil
}

// ReadText reads a NUL-terminated UTF-8 string from buf starting at *offset
// and advances *offset past the terminating 0x00 byte.
func ReadText(buf []byte, offset *int) (string, error) {
	if offset == nil || *offset < 0 || *offset > len(buf) {
		return "", errors.New("invalid text offset")
	}
	idx := bytes.IndexByte(buf[*offset:], 0)
	if idx < 0 {
		return "", fmt.Errorf("unterminated text field at offset %d: %w", *offset, io.ErrUnexpectedEOF)
	}
	s := string(buf[*offset : *offset+idx])
	*offset += idx + 1
	return s, nil
}

// ReadFixedText reads a fixed-length string of size bytes from buf starting at *offset.
func ReadFixedText(buf []byte, offset *int, size int) (string, error) {
	if offset == nil || *offset < 0 {
		return "", errors.New("invalid fixed text offset")
	}
	if size < 0 || size > len(buf)-*offset {
		return "", fmt.Errorf("fixed text truncated at offset %d (need %d bytes): %w", *offset, size, io.ErrUnexpectedEOF)
	}
	s := string(buf[*offset : *offset+size])
	*offset += size
	return s, nil
}

// ReadFixedBlob reads a fixed-length byte slice of size bytes from buf starting at *offset.
func ReadFixedBlob(buf []byte, offset *int, size int) ([]byte, error) {
	if offset == nil || *offset < 0 {
		return nil, errors.New("invalid fixed blob offset")
	}
	if size < 0 || size > len(buf)-*offset {
		return nil, fmt.Errorf("fixed blob truncated at offset %d (need %d bytes): %w", *offset, size, io.ErrUnexpectedEOF)
	}
	out := make([]byte, size)
	copy(out, buf[*offset:*offset+size])
	*offset += size
	return out, nil
}

// ReadBlob reads a VLQ-length-prefixed byte slice from buf starting at *offset.
func ReadBlob(buf []byte, offset *int) ([]byte, error) {
	size, err := ReadVLQ(buf, offset, maxVLQBytes)
	if err != nil {
		return nil, fmt.Errorf("read blob length: %w", err)
	}
	if size > uint64(len(buf)-*offset) {
		return nil, fmt.Errorf("blob truncated at offset %d (need %d bytes): %w", *offset, size, io.ErrUnexpectedEOF)
	}
	return ReadFixedBlob(buf, offset, int(size))
}
