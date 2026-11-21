package umaclient

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"path/filepath"
	"sync"
	"testing"

	"github.com/vmihailenco/msgpack/v5"

	"kudapan_lokal/internal/savedata"
)

func TestClientCallAndHeadersEndToEnd(t *testing.T) {
	store, err := savedata.LoadOrInit(filepath.Join(t.TempDir(), "savedata.json"))
	if err != nil {
		t.Fatalf("LoadOrInit() error = %v", err)
	}
	meta := store.Data()
	expectedSID := ComputeInitialSID(meta.ViewerID, meta.UDID)
	expectedSIDHex := hex.EncodeToString(expectedSID[:])
	expectedIV, err := DeriveIV(meta.UDID)
	if err != nil {
		t.Fatalf("DeriveIV() error = %v", err)
	}

	wantExactHeaders := map[string]string{
		"User-Agent":      DefaultUserAgent,
		"SID":             expectedSIDHex,
		"APP-VER":         meta.AppVer,
		"RES-VER":         meta.ResVer,
		"Content-Type":    ContentTypeMsgPack,
		"ViewerID":        meta.ViewerID,
		"Device":          DefaultDevice,
		"Device-SubType":  DefaultDeviceSubType,
		"X-Unity-Version": DefaultUnityVersion,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("Method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/umamusume/tool/get_pre_download_resource_version" {
			t.Errorf("Path = %s, want /umamusume/tool/get_pre_download_resource_version", r.URL.Path)
		}

		for k, wantVal := range wantExactHeaders {
			if got := r.Header.Get(k); got != wantVal {
				t.Errorf("r.Header.Get(%q) = %q, want %q", k, got, wantVal)
			}
		}

		bodyRaw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		packet, err := base64.StdEncoding.DecodeString(string(bodyRaw))
		if err != nil {
			t.Fatalf("base64 decode request: %v", err)
		}
		headerLen := binary.LittleEndian.Uint32(packet[:4])
		if headerLen != MinHeaderSize {
			t.Fatalf("headerLen = %d, want %d", headerLen, MinHeaderSize)
		}
		hdr, err := ParseHeader(packet[4 : 4+headerLen])
		if err != nil {
			t.Fatalf("ParseHeader() error = %v", err)
		}
		if hdr.SID != expectedSID {
			t.Error("header SID mismatch")
		}
		if hdr.UDID.String() != meta.UDID {
			t.Errorf("header UDID = %s, want %s", hdr.UDID.String(), meta.UDID)
		}

		reqMsgpack, err := DecryptBody(packet[4+headerLen:], expectedIV)
		if err != nil {
			t.Fatalf("DecryptBody(request) error = %v", err)
		}
		var reqMap map[string]any
		if err := msgpack.Unmarshal(reqMsgpack, &reqMap); err != nil {
			t.Fatalf("msgpack.Unmarshal(request) error = %v", err)
		}
		if reqMap["device"] == nil || reqMap["viewer_id"] == nil {
			t.Fatalf("missing expected keys in request map: %#v", reqMap)
		}

		respMsgpack, err := EncodePayloadMap(map[string]any{
			"data_headers": map[string]any{
				"result_code": 1,
				"sid":         "next-sid-token",
			},
			"data": map[string]any{
				"resource_version": "10008320",
			},
		})
		if err != nil {
			t.Fatalf("EncodePayloadMap(response) error = %v", err)
		}
		encResp, err := EncryptBody(respMsgpack, expectedIV, nil)
		if err != nil {
			t.Fatalf("EncryptBody(response) error = %v", err)
		}
		_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString(encResp)))
	}))
	defer srv.Close()

	client, err := New(store, WithBaseURL(srv.URL+"/umamusume"), WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var mu sync.Mutex
	wireHeaders := make(map[string][]string)
	traceCtx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		WroteHeaderField: func(key string, value []string) {
			mu.Lock()
			defer mu.Unlock()
			wireHeaders[key] = append([]string(nil), value...)
		},
	})

	resp, err := client.Call(traceCtx, "tool/get_pre_download_resource_version", map[string]any{
		"viewer_id": 0,
		"device":    4,
	})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	mu.Lock()
	for k, wantVal := range wantExactHeaders {
		vals, ok := wireHeaders[k]
		if !ok || len(vals) != 1 || vals[0] != wantVal {
			t.Errorf("wire header %q = %v (present=%v), want [%q]", k, vals, ok, wantVal)
		}
	}
	mu.Unlock()
	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("resp[data] type = %T, want map[string]any", resp["data"])
	}
	if data["resource_version"] != "10008320" {
		t.Errorf("resource_version = %v, want 10008320", data["resource_version"])
	}

	var typed struct {
		DataHeaders struct {
			ResultCode int    `msgpack:"result_code"`
			SID        string `msgpack:"sid"`
		} `msgpack:"data_headers"`
		Data struct {
			ResourceVersion string `msgpack:"resource_version"`
		} `msgpack:"data"`
	}
	if err := client.CallInto(context.Background(), "/tool/get_pre_download_resource_version", map[string]any{
		"viewer_id": 0,
		"device":    4,
	}, &typed); err != nil {
		t.Fatalf("CallInto() error = %v", err)
	}
	if typed.DataHeaders.ResultCode != 1 || typed.Data.ResourceVersion != "10008320" {
		t.Errorf("unexpected typed response: %+v", typed)
	}
}

func TestClientHTTPError(t *testing.T) {
	store, err := savedata.LoadOrInit(filepath.Join(t.TempDir(), "savedata.json"))
	if err != nil {
		t.Fatalf("LoadOrInit() error = %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer srv.Close()

	client, err := New(store, WithBaseURL(srv.URL+"/umamusume"), WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := client.Call(context.Background(), "tool/get_pre_download_resource_version", nil); err == nil {
		t.Fatal("expected error for HTTP 502 response")
	}
}
