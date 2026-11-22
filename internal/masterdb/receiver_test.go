package masterdb

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pierrec/lz4/v4"
	"github.com/vmihailenco/msgpack/v5"

	"kudapan_lokal/internal/bsv"
	"kudapan_lokal/internal/savedata"
	"kudapan_lokal/internal/umaclient"
)

func TestCalHNameVectors(t *testing.T) {
	cases := []struct {
		name     string
		size     int64
		checksum int64
		wantHash string
	}{
		{
			name:     "Windows",
			size:     684,
			checksum: 0x7dd7a79b5ad34e2e,
			wantHash: "O6K5ITIC6AGGDN4JPSTGPC4OEDKRVHE4",
		},
		{
			name:     "master",
			size:     69,
			checksum: -0x1f38614ca76c7f9b, // 0xe0c79eb358938065 signed int64
			wantHash: "D2UKHAMVZGKBJCKBYNIQL4FLQP4FGBCU",
		},
		{
			name:     "master.mdb.lz4",
			size:     5692594,
			checksum: 0x0c85559c28c30838,
			wantHash: "BCSCY2EEA4UITBRHXVQUZXFKPS6OSBKE",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CalHName(tc.checksum, tc.size, tc.name); got != tc.wantHash {
				t.Errorf("CalHName(%d, %d, %q) = %s, want %s", tc.checksum, tc.size, tc.name, got, tc.wantHash)
			}
		})
	}
}

func TestDecompressMaybeLZ4(t *testing.T) {
	rawBSV := buildSimpleBSV([]simplifiedEntry{{Name: "Windows", Size: 10, Checksum: 20}})

	gotRaw, err := DecompressMaybeLZ4(rawBSV)
	if err != nil {
		t.Fatalf("DecompressMaybeLZ4(raw) error = %v", err)
	}
	if !bytes.Equal(gotRaw, rawBSV) {
		t.Fatal("raw BSV modified unexpectedly")
	}

	compressed := compressLZ4Frame(t, rawBSV)
	gotDec, err := DecompressMaybeLZ4(compressed)
	if err != nil {
		t.Fatalf("DecompressMaybeLZ4(lz4) error = %v", err)
	}
	if !bytes.Equal(gotDec, rawBSV) {
		t.Fatal("decompressed BSV mismatch")
	}

	if _, err := DecompressMaybeLZ4([]byte("bad!")); err == nil {
		t.Fatal("expected error for invalid header")
	}
}

func TestResolveMasterURLAndDownloadEndToEnd(t *testing.T) {
	store, err := savedata.LoadOrInit(filepath.Join(t.TempDir(), "savedata.json"))
	if err != nil {
		t.Fatalf("LoadOrInit() error = %v", err)
	}
	meta := store.Data()
	iv, err := umaclient.DeriveIV(meta.UDID)
	if err != nil {
		t.Fatalf("DeriveIV() error = %v", err)
	}

	// Build fake SQLite payload & manifest chain.
	fakeSQLite := append([]byte(sqliteMagic), []byte("test-sqlite-body-data")...)
	fakeSQLiteLZ4 := compressLZ4Frame(t, fakeSQLite)
	mdbSize := int64(len(fakeSQLiteLZ4))
	mdbChecksum := int64(-1287387911940158962)
	mdbHash := CalHName(mdbChecksum, mdbSize, "master.mdb.lz4")

	masterBSV := buildMasterBSV7("master.mdb.lz4", mdbSize, mdbChecksum)
	masterBSVLZ4 := compressLZ4Frame(t, masterBSV)
	masterManifestSize := int64(len(masterBSVLZ4))
	masterManifestChecksum := int64(0x1122334455667788)
	masterManifestHash := CalHName(masterManifestChecksum, masterManifestSize, "master")

	winBSV := buildSimpleBSV([]simplifiedEntry{
		{Name: "story", Size: 100, Checksum: 1},
		{Name: "master", Size: masterManifestSize, Checksum: masterManifestChecksum},
	})
	winBSVLZ4 := compressLZ4Frame(t, winBSV)
	winSize := int64(len(winBSVLZ4))
	winChecksum := int64(-998877665544332211)
	winHash := CalHName(winChecksum, winSize, "Windows")

	rootBSV := buildSimpleBSV([]simplifiedEntry{
		{Name: "Windows", Size: winSize, Checksum: winChecksum},
		{Name: "Android", Size: 684, Checksum: 123},
	})

	const newResVer = "10009999"

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/umamusume/tool/get_pre_download_resource_version":
			cdnHost := strings.TrimPrefix(srv.URL, "http://") + "/dl/vertical/"
			respMap := map[string]any{
				"response_code": 1,
				"data_headers": map[string]any{
					"result_code": 1,
					"server_list": map[string]any{
						"resource_server": cdnHost,
					},
				},
				"data": map[string]any{
					"resource_version": newResVer,
				},
			}
			rawMsgpack, err := msgpack.Marshal(respMap)
			if err != nil {
				t.Fatalf("msgpack.Marshal() error = %v", err)
			}
			enc, err := umaclient.EncryptBody(rawMsgpack, iv, nil)
			if err != nil {
				t.Fatalf("EncryptBody() error = %v", err)
			}
			_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString(enc)))

		case fmt.Sprintf("/dl/vertical/%s/manifests/manifestdat/root.manifest.bsv", newResVer):
			_, _ = w.Write(rootBSV)

		case fmt.Sprintf("/dl/vertical/resources/Manifest/%s/%s", winHash[:2], winHash):
			_, _ = w.Write(winBSVLZ4)

		case fmt.Sprintf("/dl/vertical/resources/Manifest/%s/%s", masterManifestHash[:2], masterManifestHash):
			_, _ = w.Write(masterBSVLZ4)

		case fmt.Sprintf("/dl/vertical/resources/Generic/%s/%s", mdbHash[:2], mdbHash):
			_, _ = w.Write(fakeSQLiteLZ4)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	apiClient, err := umaclient.New(store, umaclient.WithBaseURL(srv.URL+"/umamusume"), umaclient.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("umaclient.New() error = %v", err)
	}

	// Use WithCDNBaseURL(srv.URL + "/dl/vertical/") because httptest uses http:// rather than https://.
	recv, err := New(store, apiClient,
		WithCDNBaseURL(srv.URL+"/dl/vertical/"),
		WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	wantURL := fmt.Sprintf("%s/dl/vertical/resources/Generic/%s/%s", srv.URL, mdbHash[:2], mdbHash)
	gotURL, err := recv.ResolveMasterURL(context.Background())
	if err != nil {
		t.Fatalf("ResolveMasterURL() error = %v", err)
	}
	if gotURL != wantURL {
		t.Errorf("ResolveMasterURL() = %q, want %q", gotURL, wantURL)
	}

	if gotResVer := store.Data().ResVer; gotResVer != newResVer {
		t.Errorf("savedata RES-VER = %q, want %q", gotResVer, newResVer)
	}

	outPath := filepath.Join(t.TempDir(), "nested", "master.mdb")
	info, err := recv.DownloadMaster(context.Background(), outPath)
	if err != nil {
		t.Fatalf("DownloadMaster() error = %v", err)
	}
	if info.URL != wantURL || info.HashName != mdbHash || info.ResourceVersion != newResVer {
		t.Errorf("unexpected MasterInfo: %+v", info)
	}

	gotBytes, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", outPath, err)
	}
	if !bytes.Equal(gotBytes, fakeSQLite) {
		t.Fatalf("downloaded master.mdb mismatch: got %q, want %q", gotBytes, fakeSQLite)
	}
}

func TestCheckResourceVersionDoesNotFetchManifests(t *testing.T) {
	store, err := savedata.LoadOrInit(filepath.Join(t.TempDir(), "savedata.json"))
	if err != nil {
		t.Fatalf("LoadOrInit() error = %v", err)
	}
	iv, err := umaclient.DeriveIV(store.Data().UDID)
	if err != nil {
		t.Fatalf("DeriveIV() error = %v", err)
	}

	var apiCalls, cdnCalls int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/dl/") {
			cdnCalls++
			t.Errorf("unexpected manifest/CDN request during CheckResourceVersion: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/umamusume/tool/get_pre_download_resource_version" {
			apiCalls++
			cdnHost := strings.TrimPrefix(srv.URL, "http://") + "/dl/vertical/"
			rawMsgpack, _ := msgpack.Marshal(map[string]any{
				"response_code": 1,
				"data_headers": map[string]any{
					"result_code": 1,
					"server_list": map[string]any{"resource_server": cdnHost},
				},
				"data": map[string]any{"resource_version": "10009999"},
			})
			enc, _ := umaclient.EncryptBody(rawMsgpack, iv, nil)
			_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString(enc)))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	apiClient, err := umaclient.New(store, umaclient.WithBaseURL(srv.URL+"/umamusume"), umaclient.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("umaclient.New() error = %v", err)
	}
	recv, err := New(store, apiClient, WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	rv, err := recv.CheckResourceVersion(context.Background())
	if err != nil {
		t.Fatalf("CheckResourceVersion() error = %v", err)
	}
	wantCDN := "https://" + strings.TrimPrefix(srv.URL, "http://") + "/dl/vertical/"
	if rv.ResourceVersion != "10009999" || rv.CDNBaseURL != wantCDN {
		t.Errorf("ResourceVersionInfo = %+v, want RES-VER=10009999 CDN=%q", rv, wantCDN)
	}
	if apiCalls != 1 || cdnCalls != 0 {
		t.Errorf("apiCalls=%d cdnCalls=%d, want apiCalls=1 cdnCalls=0", apiCalls, cdnCalls)
	}
}

func TestDownloadMasterRejectsNonSQLite(t *testing.T) {
	store, err := savedata.LoadOrInit(filepath.Join(t.TempDir(), "savedata.json"))
	if err != nil {
		t.Fatalf("LoadOrInit() error = %v", err)
	}
	iv, err := umaclient.DeriveIV(store.Data().UDID)
	if err != nil {
		t.Fatalf("DeriveIV() error = %v", err)
	}

	corruptLZ4 := compressLZ4Frame(t, []byte("definitely not a sqlite database file"))
	mdbHash := CalHName(1, int64(len(corruptLZ4)), "master.mdb.lz4")
	masterLZ4 := compressLZ4Frame(t, buildMasterBSV7("master.mdb.lz4", int64(len(corruptLZ4)), 1))
	masterHash := CalHName(2, int64(len(masterLZ4)), "master")
	winLZ4 := compressLZ4Frame(t, buildSimpleBSV([]simplifiedEntry{{Name: "master", Size: int64(len(masterLZ4)), Checksum: 2}}))
	winHash := CalHName(3, int64(len(winLZ4)), "Windows")
	rootBSV := buildSimpleBSV([]simplifiedEntry{{Name: "Windows", Size: int64(len(winLZ4)), Checksum: 3}})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/umamusume/tool/get_pre_download_resource_version":
			rawMsgpack, _ := msgpack.Marshal(map[string]any{
				"response_code": 1,
				"data_headers":  map[string]any{"result_code": 1},
				"data":          map[string]any{"resource_version": "10008700"},
			})
			enc, _ := umaclient.EncryptBody(rawMsgpack, iv, nil)
			_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString(enc)))
		case "/dl/vertical/10008700/manifests/manifestdat/root.manifest.bsv":
			_, _ = w.Write(rootBSV)
		case fmt.Sprintf("/dl/vertical/resources/Manifest/%s/%s", winHash[:2], winHash):
			_, _ = w.Write(winLZ4)
		case fmt.Sprintf("/dl/vertical/resources/Manifest/%s/%s", masterHash[:2], masterHash):
			_, _ = w.Write(masterLZ4)
		case fmt.Sprintf("/dl/vertical/resources/Generic/%s/%s", mdbHash[:2], mdbHash):
			_, _ = w.Write(corruptLZ4)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	apiClient, err := umaclient.New(store, umaclient.WithBaseURL(srv.URL+"/umamusume"), umaclient.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("umaclient.New() error = %v", err)
	}
	recv, err := New(store, apiClient, WithCDNBaseURL(srv.URL+"/dl/vertical/"), WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	outPath := filepath.Join(t.TempDir(), "master.mdb")
	if _, err := recv.DownloadMaster(context.Background(), outPath); err == nil {
		t.Fatal("expected DownloadMaster() to fail on non-SQLite payload")
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Fatalf("expected %q not to exist after failed download, stat err=%v", outPath, err)
	}
}

func compressLZ4Frame(t *testing.T, src []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := lz4.NewWriter(&buf)
	if _, err := zw.Write(src); err != nil {
		t.Fatalf("lz4 write error = %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("lz4 close error = %v", err)
	}
	return buf.Bytes()
}

func buildSimpleBSV(entries []simplifiedEntry) []byte {
	var rows bytes.Buffer
	for _, e := range entries {
		rows.WriteString(e.Name)
		rows.WriteByte(0)
		rows.Write(encodeVLQ(uint64(e.Size)))
		var cs [8]byte
		binary.BigEndian.PutUint64(cs[:], uint64(e.Checksum))
		rows.Write(cs[:])
	}
	schemas := []bsv.ColumnSchema{
		{Type: bsv.TypeText},
		{Type: bsv.TypeULong},
		{Type: bsv.TypeUNumFixed, FixedSize: 8},
	}
	return assembleBSV(uint64(len(entries)), 24, 32, schemas, rows.Bytes())
}

func buildMasterBSV7(name string, size, checksum int64) []byte {
	var rows bytes.Buffer
	rows.WriteString(name)
	rows.WriteByte(0)
	rows.WriteByte(0) // deps ""
	rows.WriteByte(0) // group 0
	rows.WriteByte(0) // priority 0
	rows.Write(encodeVLQ(uint64(size)))
	var cs [8]byte
	binary.BigEndian.PutUint64(cs[:], uint64(checksum))
	rows.Write(cs[:])
	rows.Write(make([]byte, 8)) // col 6 (0x13)

	schemas := []bsv.ColumnSchema{
		{Type: bsv.TypeText},
		{Type: bsv.TypeText},
		{Type: bsv.TypeUInt},
		{Type: bsv.TypeUInt},
		{Type: bsv.TypeULong},
		{Type: bsv.TypeUNumFixed, FixedSize: 8},
		{Type: bsv.TypeUInt64},
	}
	return assembleBSV(1, 48, 17, schemas, rows.Bytes())
}

func assembleBSV(rowCount, maxRowSize, schemaVersion uint64, schemas []bsv.ColumnSchema, rowsData []byte) []byte {
	var hdr bytes.Buffer
	hdr.Write(encodeVLQ(rowCount))
	hdr.Write(encodeVLQ(maxRowSize))
	hdr.Write(encodeVLQ(schemaVersion))
	hdr.Write(encodeVLQ(uint64(len(schemas))))
	for _, s := range schemas {
		hdr.WriteByte(byte(s.Type))
		if s.Type.HasFixedSize() {
			hdr.Write(encodeVLQ(s.FixedSize))
		}
	}

	var out bytes.Buffer
	out.Write(bsv.Magic[:])
	var sz [2]byte
	binary.BigEndian.PutUint16(sz[:], uint16(hdr.Len()))
	out.Write(sz[:])
	out.Write(hdr.Bytes())
	out.Write(rowsData)
	return out.Bytes()
}

func encodeVLQ(v uint64) []byte {
	var tmp [10]byte
	i := len(tmp) - 1
	tmp[i] = byte(v & 0x7f)
	v >>= 7
	for v > 0 {
		i--
		tmp[i] = byte(v&0x7f) | 0x80
		v >>= 7
	}
	return append([]byte(nil), tmp[i:]...)
}
