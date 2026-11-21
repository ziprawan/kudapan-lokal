package masterdb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pierrec/lz4/v4"

	"kudapan_lokal/internal/bsv"
)

const (
	sqliteMagic      = "SQLite format 3\x00"
	maxManifestBytes = 256 << 20
)

var lz4FrameMagic = [4]byte{0x04, 0x22, 0x4D, 0x18}

// DecompressMaybeLZ4 inspects leading magic bytes and returns raw BSV bytes.
// If data already begins with BSV magic (0xBF 0x11), it is returned as-is;
// otherwise it is decompressed as an LZ4 frame stream.
func DecompressMaybeLZ4(data []byte) ([]byte, error) {
	if len(data) < 2 {
		return nil, fmt.Errorf("manifest payload too short (%d bytes)", len(data))
	}
	if data[0] == bsv.Magic[0] && data[1] == bsv.Magic[1] {
		return data, nil
	}
	if len(data) >= 4 && !bytes.Equal(data[:4], lz4FrameMagic[:]) {
		return nil, fmt.Errorf("unexpected manifest header 0x%x (expected BSV or LZ4 frame magic)", data[:4])
	}

	lr := lz4.NewReader(bytes.NewReader(data))
	out, err := io.ReadAll(io.LimitReader(lr, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("decompress lz4 manifest: %w", err)
	}
	if len(out) > maxManifestBytes {
		return nil, fmt.Errorf("decompressed manifest exceeds %d bytes", maxManifestBytes)
	}
	return out, nil
}

func decompressSQLiteAtomic(r io.Reader, dstPath string) error {
	peek := make([]byte, len(sqliteMagic))
	n, err := io.ReadFull(r, peek)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("read master.mdb stream header: %w", err)
	}
	peek = peek[:n]

	var src io.Reader = io.MultiReader(bytes.NewReader(peek), r)
	if !bytes.Equal(peek, []byte(sqliteMagic)) {
		src = lz4.NewReader(src)
	}

	hdr := make([]byte, len(sqliteMagic))
	if _, err := io.ReadFull(src, hdr); err != nil {
		return fmt.Errorf("read decompressed sqlite header: %w", err)
	}
	if !bytes.Equal(hdr, []byte(sqliteMagic)) {
		return fmt.Errorf("downloaded payload is not a valid SQLite 3 database (header=%q)", hdr)
	}

	dir := filepath.Dir(dstPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create output directory %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".master-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp output file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(hdr); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write sqlite header: %w", err)
	}
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("stream master.mdb to disk: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp output file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp output file: %w", err)
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		return fmt.Errorf("rename temp file to %q: %w", dstPath, err)
	}
	cleanup = false
	return nil
}
