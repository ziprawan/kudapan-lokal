// Package umaclient implements the Uma Musume API wire framing, cryptography,
// and HTTP client fetcher.
package umaclient

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
)

const (
	// Salt is the static suffix appended before hashing with MD5Game.
	Salt = "co!=Y;(UQCGxJ_n82"
	// AESKeySize is the 32-byte key length used for AES-256-CBC body encryption.
	AESKeySize = 32
)

// MD5Game computes the salted MD5 digest of input using Salt.
func MD5Game(input string) [16]byte {
	return md5.Sum([]byte(input + Salt))
}

// ComputeInitialSID computes the initial request SID digest from ViewerID and
// hyphenated UDID string.
func ComputeInitialSID(viewerID, udid string) [16]byte {
	return MD5Game(viewerID + udid)
}

// DeriveIV derives the 16-byte AES-CBC initialization vector from the first 16
// hex characters of the hyphen-stripped UDID.
func DeriveIV(udid string) ([16]byte, error) {
	var iv [16]byte
	parsed, err := uuid.Parse(udid)
	if err != nil {
		return iv, fmt.Errorf("parse udid for iv: %w", err)
	}
	hexOnly := strings.ReplaceAll(parsed.String(), "-", "")
	copy(iv[:], hexOnly[:16])
	return iv, nil
}

// EncryptBody prepends a 4-byte little-endian length prefix to msgpackBytes,
// pads with PKCS#7, encrypts using AES-256-CBC with iv and key (generating a
// random 32-byte key if key is empty), and appends the key to the ciphertext.
func EncryptBody(msgpackBytes []byte, iv [16]byte, key []byte) ([]byte, error) {
	var aesKey [AESKeySize]byte
	if len(key) == 0 {
		if _, err := io.ReadFull(rand.Reader, aesKey[:]); err != nil {
			return nil, fmt.Errorf("generate aes key: %w", err)
		}
	} else if len(key) != AESKeySize {
		return nil, fmt.Errorf("invalid aes key length %d: want %d", len(key), AESKeySize)
	} else {
		copy(aesKey[:], key)
	}

	plain := make([]byte, 4+len(msgpackBytes))
	binary.LittleEndian.PutUint32(plain[:4], uint32(len(msgpackBytes)))
	copy(plain[4:], msgpackBytes)

	padded := pkcs7Pad(plain, aes.BlockSize)

	block, err := aes.NewCipher(aesKey[:])
	if err != nil {
		return nil, fmt.Errorf("create aes cipher: %w", err)
	}

	out := make([]byte, len(padded)+AESKeySize)
	cipher.NewCBCEncrypter(block, iv[:]).CryptBlocks(out[:len(padded)], padded)
	copy(out[len(padded):], aesKey[:])
	return out, nil
}

// DecryptBody splits the trailing 32-byte AES key from encryptedWithKey,
// decrypts the ciphertext with AES-256-CBC and iv, validates/removes PKCS#7
// padding, and extracts the inner MessagePack bytes.
func DecryptBody(encryptedWithKey []byte, iv [16]byte) ([]byte, error) {
	if len(encryptedWithKey) < aes.BlockSize+AESKeySize {
		return nil, fmt.Errorf("encrypted body too short: %d bytes", len(encryptedWithKey))
	}

	ctLen := len(encryptedWithKey) - AESKeySize
	if ctLen%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext length %d is not a multiple of %d", ctLen, aes.BlockSize)
	}

	ciphertext := encryptedWithKey[:ctLen]
	key := encryptedWithKey[ctLen:]

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create aes cipher: %w", err)
	}

	padded := make([]byte, ctLen)
	cipher.NewCBCDecrypter(block, iv[:]).CryptBlocks(padded, ciphertext)

	plain, err := pkcs7Unpad(padded, aes.BlockSize)
	if err != nil {
		return nil, fmt.Errorf("unpad decrypted body: %w", err)
	}

	return extractBodyMsgpack(plain)
}

func extractBodyMsgpack(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, errors.New("decrypted body is empty")
	}
	if len(plain) >= 4 {
		msgLen := binary.LittleEndian.Uint32(plain[:4])
		if msgLen > 0 && int(msgLen) == len(plain)-4 {
			return plain[4 : 4+int(msgLen)], nil
		}
	}
	if isMsgpackMapMarker(plain[0]) {
		return plain, nil
	}
	return nil, errors.New("decrypted body has invalid length prefix or non-map msgpack payload")
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	padLen := blockSize - (len(data) % blockSize)
	out := make([]byte, len(data)+padLen)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(padLen)
	}
	return out
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, errors.New("invalid padded block length")
	}
	padLen := int(data[len(data)-1])
	if padLen == 0 || padLen > blockSize || padLen > len(data) {
		return nil, errors.New("invalid pkcs7 padding length")
	}
	for i := len(data) - padLen; i < len(data); i++ {
		if data[i] != byte(padLen) {
			return nil, errors.New("invalid pkcs7 padding bytes")
		}
	}
	return data[:len(data)-padLen], nil
}

func isMsgpackMapMarker(b byte) bool {
	return (b >= 0x80 && b <= 0x8f) || b == 0xde || b == 0xdf
}
