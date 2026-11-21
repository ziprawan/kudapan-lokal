package umaclient

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vmihailenco/msgpack/v5"

	"kudapan_lokal/internal/savedata"
)

const (
	// DefaultBaseURL is the Uma Musume API base URL.
	DefaultBaseURL = "https://api.games.umamusume.com/umamusume"
	// DefaultUserAgent is the UnityPlayer User-Agent header sent by the client.
	DefaultUserAgent = "UnityPlayer/2022.3.62f2 (UnityWebRequest/1.0, libcurl/8.10.1-DEV)"
	// DefaultUnityVersion is the X-Unity-Version header value.
	DefaultUnityVersion = "2022.3.62f2"
	// DefaultDevice is the Device header value.
	DefaultDevice = "4"
	// DefaultDeviceSubType is the Device-SubType header value.
	DefaultDeviceSubType = "1"
	// ContentTypeMsgPack is the Content-Type header value for requests.
	ContentTypeMsgPack = "application/x-msgpack"

	maxResponseBytes = 64 << 20
)

// Client sends encrypted MessagePack POST requests to the Uma Musume API and
// decrypts the server responses using metadata backed by savedata.Store.
type Client struct {
	store         *savedata.Store
	baseURL       string
	httpClient    *http.Client
	userAgent     string
	unityVersion  string
	device        string
	deviceSubType string
}

// Option configures a Client instance.
type Option func(*Client)

// WithBaseURL overrides the API base URL.
func WithBaseURL(baseURL string) Option {
	return func(c *Client) {
		if baseURL != "" {
			c.baseURL = strings.TrimRight(baseURL, "/")
		}
	}
}

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithUserAgent overrides the User-Agent HTTP header.
func WithUserAgent(ua string) Option {
	return func(c *Client) {
		if ua != "" {
			c.userAgent = ua
		}
	}
}

// WithUnityVersion overrides the X-Unity-Version HTTP header.
func WithUnityVersion(version string) Option {
	return func(c *Client) {
		if version != "" {
			c.unityVersion = version
		}
	}
}

// WithDevice overrides the Device and Device-SubType HTTP headers.
func WithDevice(device, subType string) Option {
	return func(c *Client) {
		if device != "" {
			c.device = device
		}
		if subType != "" {
			c.deviceSubType = subType
		}
	}
}

type callConfig struct {
	authKey     []byte
	sidOverride *[16]byte
}

// RequestOption customizes an individual API call.
type RequestOption func(*callConfig)

// WithAuthKey attaches an optional auth_key to the in-game binary header.
func WithAuthKey(authKey []byte) RequestOption {
	return func(cfg *callConfig) {
		cfg.authKey = append([]byte(nil), authKey...)
	}
}

// WithSIDOverride overrides the computed SID digest for session-continuation calls.
func WithSIDOverride(sid [16]byte) RequestOption {
	return func(cfg *callConfig) {
		s := sid
		cfg.sidOverride = &s
	}
}

// New constructs a Client backed by store.
func New(store *savedata.Store, opts ...Option) (*Client, error) {
	if store == nil {
		return nil, errors.New("savedata store cannot be nil")
	}
	c := &Client{
		store:         store,
		baseURL:       DefaultBaseURL,
		httpClient:    &http.Client{Timeout: 30 * time.Second},
		userAgent:     DefaultUserAgent,
		unityVersion:  DefaultUnityVersion,
		device:        DefaultDevice,
		deviceSubType: DefaultDeviceSubType,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c, nil
}

// Call invokes function on the API and decodes the decrypted MessagePack
// response into map[string]any.
func (c *Client) Call(ctx context.Context, function string, payload any, reqOpts ...RequestOption) (map[string]any, error) {
	msgpackBytes, err := c.doCall(ctx, function, payload, reqOpts...)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := msgpack.Unmarshal(msgpackBytes, &out); err != nil {
		return nil, fmt.Errorf("unmarshal msgpack response from %q: %w", function, err)
	}
	return out, nil
}

// CallInto invokes function on the API and decodes the decrypted MessagePack
// response into dst.
func (c *Client) CallInto(ctx context.Context, function string, payload any, dst any, reqOpts ...RequestOption) error {
	msgpackBytes, err := c.doCall(ctx, function, payload, reqOpts...)
	if err != nil {
		return err
	}
	if err := msgpack.Unmarshal(msgpackBytes, dst); err != nil {
		return fmt.Errorf("unmarshal msgpack response from %q: %w", function, err)
	}
	return nil
}

// CallRaw invokes function on the API and returns the decrypted MessagePack bytes.
func (c *Client) CallRaw(ctx context.Context, function string, payload any, reqOpts ...RequestOption) ([]byte, error) {
	return c.doCall(ctx, function, payload, reqOpts...)
}

func (c *Client) doCall(ctx context.Context, function string, payload any, reqOpts ...RequestOption) ([]byte, error) {
	fn := strings.TrimLeft(strings.TrimSpace(function), "/")
	if fn == "" {
		return nil, errors.New("api function name cannot be empty")
	}

	var cfg callConfig
	for _, opt := range reqOpts {
		if opt != nil {
			opt(&cfg)
		}
	}

	meta := c.store.Data()
	udid, err := uuid.Parse(meta.UDID)
	if err != nil {
		return nil, fmt.Errorf("invalid savedata udid %q: %w", meta.UDID, err)
	}

	sidBytes := ComputeInitialSID(meta.ViewerID, udid.String())
	if cfg.sidOverride != nil {
		sidBytes = *cfg.sidOverride
	}
	sidHex := hex.EncodeToString(sidBytes[:])

	encodedBody, _, err := EncodeRequest(HeaderParams{
		Prefix:  DefaultPrefix,
		SID:     sidBytes,
		UDID:    udid,
		AuthKey: cfg.authKey,
	}, payload, nil)
	if err != nil {
		return nil, fmt.Errorf("encode request for %q: %w", fn, err)
	}

	endpoint := c.baseURL + "/" + fn
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(encodedBody))
	if err != nil {
		return nil, fmt.Errorf("build http request for %q: %w", fn, err)
	}

	// Assign directly to preserve exact header key casing expected by the API.
	req.Header["User-Agent"] = []string{c.userAgent}
	req.Header["SID"] = []string{sidHex}
	req.Header["APP-VER"] = []string{meta.AppVer}
	req.Header["RES-VER"] = []string{meta.ResVer}
	req.Header["Content-Type"] = []string{ContentTypeMsgPack}
	req.Header["ViewerID"] = []string{meta.ViewerID}
	req.Header["Device"] = []string{c.device}
	req.Header["Device-SubType"] = []string{c.deviceSubType}
	req.Header["X-Unity-Version"] = []string{c.unityVersion}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute request %q: %w", fn, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response body from %q: %w", fn, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("api %q returned HTTP %d: %s", fn, resp.StatusCode, truncateString(string(respBytes), 512))
	}

	msgpackBytes, err := DecodeResponseRaw(string(respBytes), udid.String())
	if err != nil {
		return nil, fmt.Errorf("decode response from %q: %w", fn, err)
	}
	return msgpackBytes, nil
}

func truncateString(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
