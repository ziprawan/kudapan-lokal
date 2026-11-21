package umaclient

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/vmihailenco/msgpack/v5"
)

const (
	// PrefixSize is the fixed 52-byte binary prefix in the in-game request header.
	PrefixSize = 52
	// MinHeaderSize is the unauthenticated header size:
	// prefix(52) + sid(16) + udid(16) + resp_key(32) = 116 bytes.
	MinHeaderSize = PrefixSize + 16 + 16 + AESKeySize
)

// DefaultPrefix is the constant 52-byte prefix prepended to in-game request headers.
var DefaultPrefix = [PrefixSize]byte{
	0x6b, 0x20, 0xe2, 0xab, 0x6c, 0x31, 0x13, 0x30, 0xf7, 0x61, 0xd7, 0x37, 0xce, 0x3f, 0x30, 0x25,
	0x75, 0x08, 0x50, 0x66, 0x5e, 0xea, 0x58, 0xb6, 0x37, 0x2f, 0x8d, 0x2f, 0x57, 0x50, 0x1e, 0xb3,
	0x44, 0xbd, 0xb7, 0x27, 0x0a, 0x90, 0x67, 0xf5, 0xb6, 0x3c, 0xd6, 0x1f, 0x15, 0x2c, 0xfb, 0x98,
	0x6c, 0xbf, 0xbf, 0x7a,
}

// HeaderParams holds the fields encoded into the in-game binary header (blob1).
type HeaderParams struct {
	Prefix      [PrefixSize]byte
	SID         [16]byte
	UDID        uuid.UUID
	ResponseKey [AESKeySize]byte
	AuthKey     []byte
}

// BuildHeader serializes HeaderParams into the binary in-game header layout.
// When AuthKey is empty, it is omitted (116 bytes total).
func BuildHeader(p HeaderParams) []byte {
	prefix := p.Prefix
	if prefix == ([PrefixSize]byte{}) {
		prefix = DefaultPrefix
	}

	out := make([]byte, MinHeaderSize+len(p.AuthKey))
	offset := 0
	offset += copy(out[offset:], prefix[:])
	offset += copy(out[offset:], p.SID[:])
	offset += copy(out[offset:], p.UDID[:])
	offset += copy(out[offset:], p.ResponseKey[:])
	if len(p.AuthKey) > 0 {
		copy(out[offset:], p.AuthKey)
	}
	return out
}

// ParseHeader decodes a binary in-game header back into HeaderParams.
func ParseHeader(raw []byte) (HeaderParams, error) {
	var p HeaderParams
	if len(raw) < MinHeaderSize {
		return p, fmt.Errorf("header too short: %d bytes, want at least %d", len(raw), MinHeaderSize)
	}
	copy(p.Prefix[:], raw[:PrefixSize])
	copy(p.SID[:], raw[PrefixSize:PrefixSize+16])
	copy(p.UDID[:], raw[PrefixSize+16:PrefixSize+32])
	copy(p.ResponseKey[:], raw[PrefixSize+32:MinHeaderSize])
	if len(raw) > MinHeaderSize {
		p.AuthKey = append([]byte(nil), raw[MinHeaderSize:]...)
	}
	return p, nil
}

// EncodePayloadMap marshals payload with MessagePack and verifies that the
// root MessagePack element is a map. A nil payload encodes as an empty map.
func EncodePayloadMap(payload any) ([]byte, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	b, err := msgpack.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal msgpack payload: %w", err)
	}
	if len(b) == 0 || !isMsgpackMapMarker(b[0]) {
		return nil, errors.New("msgpack payload root must be a map")
	}
	return b, nil
}

// EncodeRequest builds the complete Base64-encoded request body:
// base64(uint32LE(len(header)) || header || encryptedBodyWithKey).
// If params.ResponseKey is all zeros, a random 32-byte key is generated and returned.
// If bodyKey is empty, a random 32-byte AES body key is generated.
func EncodeRequest(params HeaderParams, payload any, bodyKey []byte) (string, [AESKeySize]byte, error) {
	if params.UDID == uuid.Nil {
		return "", params.ResponseKey, errors.New("header UDID cannot be nil")
	}
	if params.ResponseKey == ([AESKeySize]byte{}) {
		if _, err := io.ReadFull(rand.Reader, params.ResponseKey[:]); err != nil {
			return "", params.ResponseKey, fmt.Errorf("generate response key: %w", err)
		}
	}

	iv, err := DeriveIV(params.UDID.String())
	if err != nil {
		return "", params.ResponseKey, err
	}

	msgpackBytes, err := EncodePayloadMap(payload)
	if err != nil {
		return "", params.ResponseKey, err
	}

	encBody, err := EncryptBody(msgpackBytes, iv, bodyKey)
	if err != nil {
		return "", params.ResponseKey, err
	}

	header := BuildHeader(params)
	packet := make([]byte, 4+len(header)+len(encBody))
	binary.LittleEndian.PutUint32(packet[:4], uint32(len(header)))
	copy(packet[4:4+len(header)], header)
	copy(packet[4+len(header):], encBody)

	return base64.StdEncoding.EncodeToString(packet), params.ResponseKey, nil
}

// DecodeResponseRaw Base64-decodes encodedBase64 and decrypts the response body
// using the trailing 32-byte AES key and the IV derived from udid.
func DecodeResponseRaw(encodedBase64 string, udid string) ([]byte, error) {
	trimmed := strings.TrimSpace(encodedBase64)
	if trimmed == "" {
		return nil, errors.New("response body is empty")
	}

	raw, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("base64 decode response: %w", err)
	}

	iv, err := DeriveIV(udid)
	if err != nil {
		return nil, err
	}

	return DecryptBody(raw, iv)
}

// DecodeResponseMap decrypts encodedBase64 and unmarshals the MessagePack map
// into map[string]any.
func DecodeResponseMap(encodedBase64 string, udid string) (map[string]any, error) {
	msgpackBytes, err := DecodeResponseRaw(encodedBase64, udid)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := msgpack.Unmarshal(msgpackBytes, &out); err != nil {
		return nil, fmt.Errorf("unmarshal msgpack response: %w", err)
	}
	return out, nil
}

// DecodeResponseInto decrypts encodedBase64 and unmarshals the MessagePack
// payload into dst.
func DecodeResponseInto(encodedBase64 string, udid string, dst any) error {
	msgpackBytes, err := DecodeResponseRaw(encodedBase64, udid)
	if err != nil {
		return err
	}
	if err := msgpack.Unmarshal(msgpackBytes, dst); err != nil {
		return fmt.Errorf("unmarshal msgpack response: %w", err)
	}
	return nil
}
