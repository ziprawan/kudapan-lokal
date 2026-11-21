// Package masterdb resolves Uma Musume asset manifests and retrieves master.mdb.
package masterdb

import (
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
)

var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// CalHName computes the CDN resource hashname for a manifest entry:
// uppercase unpadded Base32(SHA-1(bigEndian(checksum) || bigEndian(length) || name)).
// Signed int64 inputs preserve exact 64-bit two's-complement bit representation.
func CalHName(checksum, length int64, name string) string {
	return CalHNameBytes(checksum, length, []byte(name))
}

// CalHNameBytes is the byte-slice variant of CalHName.
func CalHNameBytes(checksum, length int64, name []byte) string {
	buf := make([]byte, 16+len(name))
	binary.BigEndian.PutUint64(buf[0:8], uint64(checksum))
	binary.BigEndian.PutUint64(buf[8:16], uint64(length))
	copy(buf[16:], name)

	sum := sha1.Sum(buf)
	return base32NoPad.EncodeToString(sum[:])
}
