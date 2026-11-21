package umaclient

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/google/uuid"
	"github.com/vmihailenco/msgpack/v5"
)

func TestDefaultPrefixHexMatchesSpec(t *testing.T) {
	wantHex := "6b20e2ab6c311330f761d737ce3f3025" +
		"750850665eea58b6372f8d2f57501eb3" +
		"44bdb7270a9067f5b63cd61f152cfb98" +
		"6cbfbf7a"
	gotHex := hex.EncodeToString(DefaultPrefix[:])
	if gotHex != wantHex {
		t.Fatalf("DefaultPrefix = %s, want %s", gotHex, wantHex)
	}
}

func TestBuildAndParseHeader(t *testing.T) {
	udid, err := uuid.NewRandom()
	if err != nil {
		t.Fatalf("uuid.NewRandom() error = %v", err)
	}
	sid := ComputeInitialSID("0", udid.String())
	var respKey [AESKeySize]byte
	copy(respKey[:], bytes.Repeat([]byte{0xcd}, AESKeySize))

	// Unauthenticated: AuthKey omitted -> 116 bytes.
	raw := BuildHeader(HeaderParams{
		SID:         sid,
		UDID:        udid,
		ResponseKey: respKey,
	})
	if len(raw) != 116 {
		t.Fatalf("unauthenticated header len = %d, want 116", len(raw))
	}

	parsed, err := ParseHeader(raw)
	if err != nil {
		t.Fatalf("ParseHeader() error = %v", err)
	}
	if parsed.Prefix != DefaultPrefix {
		t.Error("parsed Prefix mismatch")
	}
	if parsed.SID != sid {
		t.Error("parsed SID mismatch")
	}
	if parsed.UDID != udid {
		t.Error("parsed UDID mismatch")
	}
	if parsed.ResponseKey != respKey {
		t.Error("parsed ResponseKey mismatch")
	}
	if len(parsed.AuthKey) != 0 {
		t.Errorf("parsed AuthKey len = %d, want 0", len(parsed.AuthKey))
	}

	// Authenticated: AuthKey appended -> 116 + 48 = 164 bytes.
	authKey := bytes.Repeat([]byte{0xef}, 48)
	rawAuth := BuildHeader(HeaderParams{
		SID:         sid,
		UDID:        udid,
		ResponseKey: respKey,
		AuthKey:     authKey,
	})
	if len(rawAuth) != 164 {
		t.Fatalf("authenticated header len = %d, want 164", len(rawAuth))
	}
	parsedAuth, err := ParseHeader(rawAuth)
	if err != nil {
		t.Fatalf("ParseHeader(auth) error = %v", err)
	}
	if !bytes.Equal(parsedAuth.AuthKey, authKey) {
		t.Error("parsed AuthKey mismatch")
	}
}

func TestEncodeRequestAndDecodeResponse(t *testing.T) {
	udid, err := uuid.NewRandom()
	if err != nil {
		t.Fatalf("uuid.NewRandom() error = %v", err)
	}
	sid := ComputeInitialSID("0", udid.String())

	payload := map[string]any{
		"viewer_id": 0,
		"device":    4,
	}

	encoded, respKey, err := EncodeRequest(HeaderParams{
		SID:  sid,
		UDID: udid,
	}, payload, nil)
	if err != nil {
		t.Fatalf("EncodeRequest() error = %v", err)
	}
	if respKey == ([AESKeySize]byte{}) {
		t.Fatal("expected generated ResponseKey to be non-zero")
	}

	packet, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 decode error = %v", err)
	}
	headerLen := binary.LittleEndian.Uint32(packet[:4])
	if headerLen != 116 {
		t.Fatalf("headerLen = %d, want 116", headerLen)
	}

	hdr, err := ParseHeader(packet[4 : 4+headerLen])
	if err != nil {
		t.Fatalf("ParseHeader() error = %v", err)
	}
	if hdr.ResponseKey != respKey {
		t.Fatal("header ResponseKey mismatch")
	}

	iv, err := DeriveIV(udid.String())
	if err != nil {
		t.Fatalf("DeriveIV() error = %v", err)
	}
	decMsgpack, err := DecryptBody(packet[4+headerLen:], iv)
	if err != nil {
		t.Fatalf("DecryptBody() error = %v", err)
	}
	var decodedPayload map[string]any
	if err := msgpack.Unmarshal(decMsgpack, &decodedPayload); err != nil {
		t.Fatalf("msgpack.Unmarshal() error = %v", err)
	}
	if decodedPayload["device"] == nil || decodedPayload["viewer_id"] == nil {
		t.Fatalf("unexpected decoded payload: %#v", decodedPayload)
	}

	// Verify DecodeResponseMap with an encrypted response body.
	respBytes, err := EncodePayloadMap(map[string]any{
		"data_headers": map[string]any{"result_code": 1},
		"data":         map[string]any{"resource_version": "10008320"},
	})
	if err != nil {
		t.Fatalf("EncodePayloadMap() error = %v", err)
	}
	encResp, err := EncryptBody(respBytes, iv, nil)
	if err != nil {
		t.Fatalf("EncryptBody() error = %v", err)
	}
	gotMap, err := DecodeResponseMap(base64.StdEncoding.EncodeToString(encResp), udid.String())
	if err != nil {
		t.Fatalf("DecodeResponseMap() error = %v", err)
	}
	headers, ok := gotMap["data_headers"].(map[string]any)
	if !ok {
		t.Fatalf("data_headers type = %T, want map[string]any", gotMap["data_headers"])
	}
	if headers["result_code"] == nil {
		t.Fatalf("missing result_code in %#v", headers)
	}
}

func TestEncodePayloadMapRejectsNonMapRoot(t *testing.T) {
	if _, err := EncodePayloadMap([]int{1, 2, 3}); err == nil {
		t.Error("expected error when root payload is a slice")
	}
	if _, err := EncodePayloadMap("hello"); err == nil {
		t.Error("expected error when root payload is a string")
	}
	empty, err := EncodePayloadMap(nil)
	if err != nil {
		t.Fatalf("EncodePayloadMap(nil) error = %v", err)
	}
	if !bytes.Equal(empty, []byte{0x80}) {
		t.Fatalf("EncodePayloadMap(nil) = %x, want 80", empty)
	}
}
