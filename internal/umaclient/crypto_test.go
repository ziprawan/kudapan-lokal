package umaclient

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/hex"
	"testing"

	"github.com/google/uuid"
)

func TestMD5GameAndComputeInitialSID(t *testing.T) {
	udid := "01950000-0000-7000-8000-000000000001"
	viewerID := "0"

	got := ComputeInitialSID(viewerID, udid)
	want := md5.Sum([]byte(viewerID + udid + "co!=Y;(UQCGxJ_n82"))
	if got != want {
		t.Fatalf("ComputeInitialSID() = %x, want %x", got, want)
	}
	if len(hex.EncodeToString(got[:])) != 32 {
		t.Fatalf("expected 32 hex chars")
	}
}

func TestDeriveIV(t *testing.T) {
	udid := "01951234-5678-7abc-8def-0123456789ab"
	iv, err := DeriveIV(udid)
	if err != nil {
		t.Fatalf("DeriveIV() error = %v", err)
	}
	want := "0195123456787abc"
	if string(iv[:]) != want {
		t.Errorf("DeriveIV() = %q, want %q", string(iv[:]), want)
	}

	if _, err := DeriveIV("not-a-uuid"); err == nil {
		t.Error("expected DeriveIV to fail on invalid UUID")
	}
}

func TestPKCS7RoundTripAndBlockBoundary(t *testing.T) {
	for _, length := range []int{0, 1, 15, 16, 17, 31, 32} {
		input := bytes.Repeat([]byte{0x42}, length)
		padded := pkcs7Pad(input, aes.BlockSize)
		if len(padded)%aes.BlockSize != 0 || len(padded) == 0 {
			t.Fatalf("length %d: padded len %d invalid", length, len(padded))
		}
		unpadded, err := pkcs7Unpad(padded, aes.BlockSize)
		if err != nil {
			t.Fatalf("length %d: pkcs7Unpad error = %v", length, err)
		}
		if !bytes.Equal(unpadded, input) {
			t.Fatalf("length %d: roundtrip mismatch", length)
		}
	}

	bad := make([]byte, aes.BlockSize)
	bad[aes.BlockSize-1] = 2
	bad[aes.BlockSize-2] = 99
	if _, err := pkcs7Unpad(bad, aes.BlockSize); err == nil {
		t.Error("expected error on invalid pkcs7 padding bytes")
	}
}

func TestEncryptDecryptBodyRoundTrip(t *testing.T) {
	u, err := uuid.NewRandom()
	if err != nil {
		t.Fatalf("uuid.NewRandom() error = %v", err)
	}
	iv, err := DeriveIV(u.String())
	if err != nil {
		t.Fatalf("DeriveIV() error = %v", err)
	}

	msgpackPayload := []byte{0x81, 0xa3, 'f', 'o', 'o', 0x01}
	key := bytes.Repeat([]byte{0xab}, AESKeySize)

	enc, err := EncryptBody(msgpackPayload, iv, key)
	if err != nil {
		t.Fatalf("EncryptBody() error = %v", err)
	}
	if !bytes.Equal(enc[len(enc)-AESKeySize:], key) {
		t.Fatal("trailing 32 bytes do not match encryption key")
	}

	dec, err := DecryptBody(enc, iv)
	if err != nil {
		t.Fatalf("DecryptBody() error = %v", err)
	}
	if !bytes.Equal(dec, msgpackPayload) {
		t.Fatalf("DecryptBody() = %x, want %x", dec, msgpackPayload)
	}

	// Verify unframed msgpack map decryption fallback as well.
	block, _ := aes.NewCipher(key)
	paddedUnframed := pkcs7Pad(msgpackPayload, aes.BlockSize)
	unframedEnc := make([]byte, len(paddedUnframed)+AESKeySize)
	cipher.NewCBCEncrypter(block, iv[:]).CryptBlocks(unframedEnc[:len(paddedUnframed)], paddedUnframed)
	copy(unframedEnc[len(paddedUnframed):], key)

	decUnframed, err := DecryptBody(unframedEnc, iv)
	if err != nil {
		t.Fatalf("DecryptBody(unframed) error = %v", err)
	}
	if !bytes.Equal(decUnframed, msgpackPayload) {
		t.Fatalf("DecryptBody(unframed) = %x, want %x", decUnframed, msgpackPayload)
	}

	if _, err := DecryptBody([]byte("short"), iv); err == nil {
		t.Error("expected error for short ciphertext")
	}
}
