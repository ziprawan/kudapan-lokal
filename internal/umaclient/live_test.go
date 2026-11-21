package umaclient

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"kudapan_lokal/internal/savedata"
)

func TestLiveGetPreDownloadResourceVersion(t *testing.T) {
	if os.Getenv("UMA_LIVE_TEST") != "1" {
		t.Skip("skipping live API test; set UMA_LIVE_TEST=1 to run against api.games.umamusume.com")
	}

	store, err := savedata.LoadOrInit(filepath.Join(t.TempDir(), "savedata.json"))
	if err != nil {
		t.Fatalf("LoadOrInit() error = %v", err)
	}
	meta := store.Data()
	viewerID, err := strconv.ParseInt(meta.ViewerID, 10, 64)
	if err != nil {
		t.Fatalf("parse ViewerID %q: %v", meta.ViewerID, err)
	}

	client, err := New(store)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	payload := map[string]any{
		"viewer_id": viewerID,
		"device":    4,
	}

	resp, err := client.Call(ctx, "tool/get_pre_download_resource_version", payload)
	if err != nil {
		t.Fatalf("live Call() error = %v", err)
	}

	t.Logf("savedata UDID=%s APP-VER=%s RES-VER=%s", meta.UDID, meta.AppVer, meta.ResVer)
	t.Logf("decoded server response: %#v", resp)

	headers, ok := resp["data_headers"].(map[string]any)
	if !ok {
		t.Fatalf("expected data_headers map in response, got %T (%#v)", resp["data_headers"], resp)
	}
	resultCode, ok := asInt64(headers["result_code"])
	if !ok || resultCode != 1 {
		t.Fatalf("data_headers.result_code = %v (ok=%v), want 1 (full response: %#v)", headers["result_code"], ok, resp)
	}

	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected data map in response, got %T (%#v)", resp["data"], resp)
	}
	resVer, ok := data["resource_version"].(string)
	if !ok || resVer == "" {
		t.Fatalf("expected non-empty data.resource_version string, got %#v", data["resource_version"])
	}

	var typed struct {
		ResponseCode int `msgpack:"response_code"`
		DataHeaders  struct {
			ResultCode int   `msgpack:"result_code"`
			ServerTime int64 `msgpack:"servertime"`
			ViewerID   int64 `msgpack:"viewer_id"`
		} `msgpack:"data_headers"`
		Data struct {
			ResourceVersion string `msgpack:"resource_version"`
		} `msgpack:"data"`
	}
	if err := client.CallInto(ctx, "tool/get_pre_download_resource_version", payload, &typed); err != nil {
		t.Fatalf("live CallInto() error = %v", err)
	}
	if typed.ResponseCode != 1 || typed.DataHeaders.ResultCode != 1 || typed.Data.ResourceVersion == "" {
		t.Fatalf("unexpected typed response: %+v", typed)
	}
}

func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint:
		return int64(n), true
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		return int64(n), true
	default:
		return 0, false
	}
}
