package masterdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kudapan_lokal/internal/bsv"
	"kudapan_lokal/internal/savedata"
	"kudapan_lokal/internal/umaclient"
)

const (
	// DefaultCDNBaseURL is the fallback CDN base URL when server_list is empty.
	DefaultCDNBaseURL = "https://assets-umamusume-en.akamaized.net/dl/vertical/"
	// DefaultPlatform is the default platform entry selected from root.manifest.bsv.
	DefaultPlatform = "Windows"
	// DefaultOutputPath is used by DownloadMaster when dstPath is empty.
	DefaultOutputPath = "master.mdb"
	// DefaultDevice is the default device ID sent to tool/get_pre_download_resource_version.
	DefaultDevice = 4

	masterManifestEntryName = "master"
	masterDBAssetName       = "master.mdb.lz4"
)

// MasterInfo describes the resolved master.mdb.lz4 asset location and metadata.
type MasterInfo struct {
	ResourceVersion string
	CDNBaseURL      string
	Platform        string
	HashName        string
	URL             string
	CompressedSize  int64
	Checksum        int64
}

type manifestEntry struct {
	Name     string
	Size     int64
	Checksum int64
}

type simplifiedEntry struct {
	Name     string
	Size     int64
	Checksum int64
}

type assetEntry6 struct {
	Name     string
	Deps     string
	Group    uint32
	Priority uint32
	Size     int64
	Checksum int64
}

type assetEntry7 struct {
	Name     string
	Deps     string
	Group    uint32
	Priority uint32
	Size     int64
	Checksum int64
	Extra    uint64
}

type preDownloadResponse struct {
	ResponseCode int `msgpack:"response_code"`
	DataHeaders  struct {
		ResultCode int `msgpack:"result_code"`
		ServerList struct {
			ResourceServer string `msgpack:"resource_server"`
		} `msgpack:"server_list"`
	} `msgpack:"data_headers"`
	Data struct {
		ResourceVersion string `msgpack:"resource_version"`
	} `msgpack:"data"`
}

// Receiver resolves and downloads master.mdb via the Uma Musume API and CDN.
type Receiver struct {
	store      *savedata.Store
	client     *umaclient.Client
	httpClient *http.Client
	cdnBaseURL string
	platform   string
	device     int
	logger     *log.Logger
}

// Option configures a Receiver instance.
type Option func(*Receiver)

// WithLogger attaches an optional logger for verbose manifest/download progress.
func WithLogger(l *log.Logger) Option {
	return func(r *Receiver) {
		r.logger = l
	}
}

// WithCDNBaseURL overrides the CDN base URL instead of reading server_list.resource_server.
func WithCDNBaseURL(baseURL string) Option {
	return func(r *Receiver) {
		if baseURL != "" {
			r.cdnBaseURL = normalizeCDNBaseURL(baseURL)
		}
	}
}

// WithHTTPClient overrides the *http.Client used for unencrypted CDN downloads.
func WithHTTPClient(hc *http.Client) Option {
	return func(r *Receiver) {
		if hc != nil {
			r.httpClient = hc
		}
	}
}

// WithPlatform overrides the platform selected in root.manifest.bsv (default "Windows").
func WithPlatform(platform string) Option {
	return func(r *Receiver) {
		if strings.TrimSpace(platform) != "" {
			r.platform = strings.TrimSpace(platform)
		}
	}
}

// WithDevice overrides the integer device parameter sent in API requests (default 4).
func WithDevice(device int) Option {
	return func(r *Receiver) {
		if device > 0 {
			r.device = device
		}
	}
}

// New constructs a Receiver backed by store and client.
// If client is nil, a default umaclient.Client is created from store.
func New(store *savedata.Store, client *umaclient.Client, opts ...Option) (*Receiver, error) {
	if store == nil {
		return nil, errors.New("savedata store cannot be nil")
	}
	if client == nil {
		var err error
		client, err = umaclient.New(store)
		if err != nil {
			return nil, fmt.Errorf("create default umaclient: %w", err)
		}
	}

	r := &Receiver{
		store:      store,
		client:     client,
		httpClient: &http.Client{Timeout: 60 * time.Second},
		platform:   DefaultPlatform,
		device:     DefaultDevice,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	return r, nil
}

// ResolveMasterURL resolves the current RES-VER and manifest chain, returning
// the direct CDN URL of master.mdb.lz4.
func (r *Receiver) ResolveMasterURL(ctx context.Context) (string, error) {
	info, err := r.ResolveMaster(ctx)
	if err != nil {
		return "", err
	}
	return info.URL, nil
}

func (r *Receiver) logf(format string, args ...any) {
	if r.logger != nil {
		r.logger.Printf("[masterdb] "+format, args...)
	}
}

// ResolveMaster fetches the latest RES-VER, updates savedata.Store if changed,
// traverses root -> platform -> master manifests, and returns resolved MasterInfo.
func (r *Receiver) ResolveMaster(ctx context.Context) (*MasterInfo, error) {
	meta := r.store.Data()
	viewerID, err := strconv.ParseInt(meta.ViewerID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse savedata viewer_id %q: %w", meta.ViewerID, err)
	}

	r.logf("calling tool/get_pre_download_resource_version (viewer_id=%d, device=%d, current_res_ver=%s)",
		viewerID, r.device, meta.ResVer)
	var apiResp preDownloadResponse
	if err := r.client.CallInto(ctx, "tool/get_pre_download_resource_version", map[string]any{
		"viewer_id": viewerID,
		"device":    r.device,
	}, &apiResp); err != nil {
		return nil, fmt.Errorf("fetch resource version: %w", err)
	}
	if apiResp.DataHeaders.ResultCode != 1 {
		return nil, fmt.Errorf("tool/get_pre_download_resource_version returned result_code=%d (response_code=%d)",
			apiResp.DataHeaders.ResultCode, apiResp.ResponseCode)
	}

	resVer := strings.TrimSpace(apiResp.Data.ResourceVersion)
	if resVer == "" {
		return nil, errors.New("tool/get_pre_download_resource_version returned empty resource_version")
	}

	if resVer != meta.ResVer {
		if err := r.store.Update(func(d *savedata.Data) error {
			d.ResVer = resVer
			return nil
		}); err != nil {
			return nil, fmt.Errorf("persist updated RES-VER %q: %w", resVer, err)
		}
		r.logf("updated savedata RES-VER: %s -> %s", meta.ResVer, resVer)
	}

	cdnBase := r.cdnBaseURL
	if cdnBase == "" {
		cdnBase = normalizeCDNBaseURL(apiResp.DataHeaders.ServerList.ResourceServer)
	}
	if cdnBase == "" {
		cdnBase = DefaultCDNBaseURL
	}

	// 1. Fetch root.manifest.bsv and select platform entry (default "Windows").
	rootURL := fmt.Sprintf("%s%s/manifests/manifestdat/root.manifest.bsv", cdnBase, resVer)
	r.logf("fetching root manifest: %s", rootURL)
	rootEntries, err := r.fetchManifestEntries(ctx, rootURL)
	if err != nil {
		return nil, fmt.Errorf("fetch root manifest (%s): %w", rootURL, err)
	}
	platformEntry, ok := findEntry(rootEntries, r.platform)
	if !ok {
		return nil, fmt.Errorf("platform %q not found in root manifest", r.platform)
	}

	// 2. Fetch platform manifest and select "master" entry.
	platformHash := CalHName(platformEntry.Checksum, platformEntry.Size, platformEntry.Name)
	platformURL := manifestCDNURL(cdnBase, platformHash)
	r.logf("fetching %s manifest (%d root entries): %s", r.platform, len(rootEntries), platformURL)
	platformEntries, err := r.fetchManifestEntries(ctx, platformURL)
	if err != nil {
		return nil, fmt.Errorf("fetch %s manifest (%s): %w", r.platform, platformURL, err)
	}
	masterManifestEntry, ok := findEntry(platformEntries, masterManifestEntryName)
	if !ok {
		return nil, fmt.Errorf("%q entry not found in %s manifest", masterManifestEntryName, r.platform)
	}

	// 3. Fetch master manifest and select "master.mdb.lz4" entry.
	masterHash := CalHName(masterManifestEntry.Checksum, masterManifestEntry.Size, masterManifestEntry.Name)
	masterManifestURL := manifestCDNURL(cdnBase, masterHash)
	r.logf("fetching master manifest (%d %s entries): %s", len(platformEntries), r.platform, masterManifestURL)
	masterEntries, err := r.fetchManifestEntries(ctx, masterManifestURL)
	if err != nil {
		return nil, fmt.Errorf("fetch master manifest (%s): %w", masterManifestURL, err)
	}
	mdbEntry, ok := findEntry(masterEntries, masterDBAssetName)
	if !ok {
		return nil, fmt.Errorf("%q entry not found in master manifest", masterDBAssetName)
	}

	mdbHash := CalHName(mdbEntry.Checksum, mdbEntry.Size, mdbEntry.Name)
	mdbURL := genericCDNURL(cdnBase, mdbHash)
	r.logf("resolved %s (hash=%s, compressed_size=%d bytes, url=%s)",
		masterDBAssetName, mdbHash, mdbEntry.Size, mdbURL)

	return &MasterInfo{
		ResourceVersion: resVer,
		CDNBaseURL:      cdnBase,
		Platform:        platformEntry.Name,
		HashName:        mdbHash,
		URL:             mdbURL,
		CompressedSize:  mdbEntry.Size,
		Checksum:        mdbEntry.Checksum,
	}, nil
}

// DownloadMaster resolves the master.mdb.lz4 URL, downloads it, decompresses LZ4,
// validates the SQLite 3 header, and writes the database atomically to dstPath.
func (r *Receiver) DownloadMaster(ctx context.Context, dstPath string) (*MasterInfo, error) {
	if strings.TrimSpace(dstPath) == "" {
		dstPath = DefaultOutputPath
	}

	info, err := r.ResolveMaster(ctx)
	if err != nil {
		return nil, err
	}

	r.logf("downloading %s (%d compressed bytes) -> %q", info.URL, info.CompressedSize, dstPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("build master.mdb request: %w", err)
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download master.mdb.lz4: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("download master.mdb.lz4 (%s) returned HTTP %d: %s",
			info.URL, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	if err := decompressSQLiteAtomic(resp.Body, dstPath); err != nil {
		return nil, fmt.Errorf("save master.mdb to %q: %w", dstPath, err)
	}
	r.logf("saved decompressed SQLite database to %q", dstPath)
	return info, nil
}

func (r *Receiver) fetchManifestEntries(ctx context.Context, url string) ([]manifestEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 512)])))
	}
	if len(body) > maxManifestBytes {
		return nil, fmt.Errorf("compressed manifest exceeds %d bytes", maxManifestBytes)
	}

	bsvData, err := DecompressMaybeLZ4(body)
	if err != nil {
		return nil, err
	}
	return decodeManifestEntries(bsvData)
}

func decodeManifestEntries(bsvData []byte) ([]manifestEntry, error) {
	rd, err := bsv.NewReader(bsvData)
	if err != nil {
		return nil, err
	}
	cols := len(rd.Header().Schemas)

	switch cols {
	case 3:
		rows, err := bsv.Decode[simplifiedEntry](bsvData)
		if err != nil {
			return nil, err
		}
		out := make([]manifestEntry, len(rows))
		for i, row := range rows {
			out[i] = manifestEntry(row)
		}
		return out, nil

	case 6:
		rows, err := bsv.Decode[assetEntry6](bsvData)
		if err != nil {
			return nil, err
		}
		out := make([]manifestEntry, len(rows))
		for i, row := range rows {
			out[i] = manifestEntry{Name: row.Name, Size: row.Size, Checksum: row.Checksum}
		}
		return out, nil

	case 7:
		rows, err := bsv.Decode[assetEntry7](bsvData)
		if err != nil {
			return nil, err
		}
		out := make([]manifestEntry, len(rows))
		for i, row := range rows {
			out[i] = manifestEntry{Name: row.Name, Size: row.Size, Checksum: row.Checksum}
		}
		return out, nil

	default:
		return nil, fmt.Errorf("unsupported manifest column count %d", cols)
	}
}

func findEntry(entries []manifestEntry, name string) (manifestEntry, bool) {
	for _, e := range entries {
		if e.Name == name {
			return e, true
		}
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name, name) {
			return e, true
		}
	}
	return manifestEntry{}, false
}

func manifestCDNURL(cdnBase, hashName string) string {
	return fmt.Sprintf("%sresources/Manifest/%s/%s", cdnBase, hashName[:2], hashName)
}

func genericCDNURL(cdnBase, hashName string) string {
	return fmt.Sprintf("%sresources/Generic/%s/%s", cdnBase, hashName[:2], hashName)
}

func normalizeCDNBaseURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "https://" + strings.TrimLeft(s, "/")
	}
	if !strings.HasSuffix(s, "/") {
		s += "/"
	}
	return s
}
