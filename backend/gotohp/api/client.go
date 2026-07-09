package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RequiredAuthFields are the fields gotohp's own "creds add" validates for
// (see backend/configmanager.go's AddCredentials in xob0t/gotohp). "Token",
// "service" and "it_caveat_types"/"assertion_jwt" relate to the rooted-device
// token-binding flow, which this client does not implement (see ParseAuth).
var RequiredAuthFields = []string{"androidId", "app", "client_sig", "Email", "Token", "lang", "service"}

// ParseAuth validates the raw credential blob captured from the Google
// Photos Android app and returns its parsed fields.
func ParseAuth(raw string) (url.Values, error) {
	params, err := url.ParseQuery(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("gotohp: invalid auth string: %w", err)
	}
	var missing []string
	for _, f := range RequiredAuthFields {
		if params.Get(f) == "" {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("gotohp: auth string missing required field(s): %v", missing)
	}
	return params, nil
}

const (
	androidAPIVersion = 28
	deviceModel       = "Pixel XL"
	deviceMake        = "Google"
	clientVersionCode = 49029607
	packageName       = "com.google.android.apps.photos"

	defaultUploadEndpoint      = "https://photos.googleapis.com/data/upload/uploadmedia/interactive"
	defaultHashCheckEndpoint   = "https://photosdata-pa.googleapis.com/6439526531001121323/5084965799730810217"
	defaultCommitEndpoint      = "https://photosdata-pa.googleapis.com/6439526531001121323/16538846908252377752"
	defaultCreateAlbumEndpoint = "https://photosdata-pa.googleapis.com/6439526531001121323/8386163679468898444"
	defaultAddToAlbumEndpoint  = "https://photosdata-pa.googleapis.com/6439526531001121323/484917746253879292"
	defaultAuthEndpoint        = "https://android.googleapis.com/auth"
)

// Quality selects the upload quality tier. It mirrors gotohp's own
// behaviour of picking it via a spoofed device model rather than an
// explicit API parameter.
type Quality int

const (
	QualityOriginal Quality = iota
	QualityStorageSaver
)

// NewHTTPClient builds an http.Client suitable for large, long-running
// uploads (no aggregate timeout — callers rely on context for
// cancellation). rclone's shared fs/fshttp client is deliberately not used
// here: its Transport unconditionally overwrites the User-Agent header on
// every request, which would break this protocol's requirement to present
// as a specific spoofed Android Google Photos client.
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 10
	transport.MaxConnsPerHost = 10
	transport.IdleConnTimeout = 90 * time.Second
	return &http.Client{Transport: transport}
}

// Client talks to the unofficial Google Photos native (Android app) upload
// API using a captured device credential.
type Client struct {
	httpClient *http.Client
	authRaw    string
	language   string
	userAgent  string
	quality    Quality
	useQuota   bool

	// Endpoint URLs, overridable (package-internal only) for testing against
	// an httptest server instead of the real Google endpoints.
	authEndpoint        string
	uploadEndpoint      string
	hashCheckEndpoint   string
	commitEndpoint      string
	createAlbumEndpoint string
	addToAlbumEndpoint  string

	mu           sync.Mutex
	cachedBearer string
	cachedExpiry int64
}

// NewClient parses and validates authRaw and prepares a Client. It performs
// no network I/O; the credential is only exercised on first real API call.
func NewClient(httpClient *http.Client, authRaw string, quality Quality, useQuota bool) (*Client, error) {
	params, err := ParseAuth(authRaw)
	if err != nil {
		return nil, err
	}
	lang := params.Get("lang")
	c := &Client{
		httpClient:          httpClient,
		authRaw:             strings.TrimSpace(authRaw),
		language:            lang,
		quality:             quality,
		useQuota:            useQuota,
		authEndpoint:        defaultAuthEndpoint,
		uploadEndpoint:      defaultUploadEndpoint,
		hashCheckEndpoint:   defaultHashCheckEndpoint,
		commitEndpoint:      defaultCommitEndpoint,
		createAlbumEndpoint: defaultCreateAlbumEndpoint,
		addToAlbumEndpoint:  defaultAddToAlbumEndpoint,
	}
	c.userAgent = fmt.Sprintf(
		"com.google.android.apps.photos/%d (Linux; U; Android 9; %s; %s; Build/PQ2A.190205.001; Cronet/127.0.6510.5) (gzip)",
		clientVersionCode, lang, deviceModel,
	)
	return c, nil
}

// deviceIdentity returns the (model, make) pair gotohp spoofs for the
// current quality/quota mode: quality=storage-saver -> "Pixel 2",
// use-quota -> "Pixel 8", otherwise the default "Pixel XL".
func (c *Client) deviceIdentity() (model, make string) {
	switch {
	case c.useQuota:
		return "Pixel 8", deviceMake
	case c.quality == QualityStorageSaver:
		return "Pixel 2", deviceMake
	default:
		return deviceModel, deviceMake
	}
}

func readBody(resp *http.Response) ([]byte, error) {
	var r io.Reader = resp.Body
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Encoding")), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("gotohp: gzip decode failed: %w", err)
		}
		defer func() { _ = gz.Close() }()
		r = gz
	}
	return io.ReadAll(r)
}

func backoff(attempt int) time.Duration {
	d := time.Second * time.Duration(int64(1)<<uint(attempt))
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// bearerToken returns a cached bearer token, refreshing it via the auth
// endpoint if expired. Note: this client only supports the non-rooted
// credential flow (no ECDSA token-binding); accounts that require binding
// will fail here with a clear error rather than being silently mishandled.
func (c *Client) bearerToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cachedBearer != "" && c.cachedExpiry > time.Now().Unix() {
		return c.cachedBearer, nil
	}
	token, expiry, err := c.getAuthToken(ctx)
	if err != nil {
		return "", err
	}
	c.cachedBearer = token
	c.cachedExpiry = expiry
	return token, nil
}

func (c *Client) getAuthToken(ctx context.Context) (token string, expiry int64, err error) {
	authValues, err := url.ParseQuery(c.authRaw)
	if err != nil {
		return "", 0, fmt.Errorf("gotohp: invalid auth string: %w", err)
	}
	reqValues := url.Values{}
	for k, v := range authValues {
		reqValues[k] = append([]string(nil), v...)
	}
	reqValues.Set("app", packageName)
	reqValues.Set("callerPkg", packageName)
	// Rooted-device token-binding assertion is not implemented; if the
	// credential requests it, drop the alias so the server either succeeds
	// without it or fails with a clear rejection rather than us attempting
	// (and getting wrong) an ECDSA assertion.
	reqValues.Del("it_caveat_types")
	reqValues.Del("assertion_jwt")
	needsBinding := reqValues.Get("token_binding_alias") != ""
	reqValues.Del("token_binding_alias")

	req, err := http.NewRequestWithContext(ctx, "POST", c.authEndpoint, strings.NewReader(reqValues.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("app", packageName)
	req.Header.Set("Connection", "Keep-Alive")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("device", reqValues.Get("androidId"))
	req.Header.Set("User-Agent", "GoogleAuth/1.4 (Pixel XL PQ2A.190205.001); gzip")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("gotohp: auth request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readBody(resp)
	if err != nil {
		return "", 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("gotohp: auth request failed with status %d: %s", resp.StatusCode, body)
	}

	parsed := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if kv := strings.SplitN(line, "=", 2); len(kv) == 2 {
			parsed[kv[0]] = kv[1]
		}
	}
	if parsed["Auth"] == "" {
		if needsBinding {
			return "", 0, errors.New("gotohp: this account requires rooted-device token binding, which is not supported by this backend")
		}
		return "", 0, errors.New("gotohp: auth response missing Auth token (credential may have expired — recapture it)")
	}
	expiryVal, _ := strconv.ParseInt(parsed["Expiry"], 10, 64)
	return parsed["Auth"], expiryVal, nil
}

// postProtobuf issues a POST with an application/x-protobuf body and
// returns the (possibly gzip-decoded) response body.
func (c *Client) postProtobuf(ctx context.Context, endpoint string, body []byte, extHeaders bool) ([]byte, error) {
	bearer, err := c.bearerToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Accept-Language", c.language)
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Authorization", "Bearer "+bearer)
	if extHeaders {
		req.Header.Set("X-Goog-Ext-173412678-Bin", "CgcIAhClARgC")
		req.Header.Set("X-Goog-Ext-174067345-Bin", "CgIIAg==")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gotohp: request to %s failed: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := readBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gotohp: request to %s failed with status %d: %s", endpoint, resp.StatusCode, respBody)
	}
	return respBody, nil
}

// GetUploadToken obtains an upload token for a file of the given size/hash.
// This is step 1 of the 3-step upload dance; the token must be redeemed via
// UploadBytes and the byte stream's CommitToken finalized via CommitUpload,
// or the bytes become an orphaned upload that never becomes a photo.
func (c *Client) GetUploadToken(ctx context.Context, sha1Hash []byte, fileSize int64) (string, error) {
	bearer, err := c.bearerToken(ctx)
	if err != nil {
		return "", err
	}
	body := EncodeGetUploadToken(fileSize)
	req, err := http.NewRequestWithContext(ctx, "POST", c.uploadEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Accept-Language", c.language)
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("X-Goog-Hash", "sha1="+base64.StdEncoding.EncodeToString(sha1Hash))
	req.Header.Set("X-Upload-Content-Length", strconv.FormatInt(fileSize, 10))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("gotohp: upload token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err = readBody(resp)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("gotohp: upload token request failed with status %d: %s", resp.StatusCode, body)
	}
	token := resp.Header.Get("X-GUploader-UploadID")
	if token == "" {
		return "", errors.New("gotohp: response missing X-GUploader-UploadID header")
	}
	return token, nil
}

// UploadBytes streams the content served by open (step 2) and returns the
// opaque CommitToken to finalize via CommitUpload. open is called fresh on
// each retry attempt so the source doesn't need to be seekable — it must
// just be freshly-openable (a spooled local file, as gotohp itself uses).
func (c *Client) UploadBytes(ctx context.Context, uploadToken string, open func() (io.ReadCloser, error)) (CommitToken, error) {
	const maxRetries = 3
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return CommitToken{}, ctx.Err()
			case <-time.After(backoff(attempt - 1)):
			}
		}
		r, err := open()
		if err != nil {
			return CommitToken{}, fmt.Errorf("gotohp: reopening spooled upload failed: %w", err)
		}
		tok, err := c.doUploadRequest(ctx, uploadToken, r)
		_ = r.Close()
		if err == nil {
			return tok, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return CommitToken{}, ctx.Err()
		}
	}
	return CommitToken{}, fmt.Errorf("gotohp: upload failed after %d attempts: %w", maxRetries+1, lastErr)
}

func (c *Client) doUploadRequest(ctx context.Context, uploadToken string, r io.Reader) (CommitToken, error) {
	bearer, err := c.bearerToken(ctx)
	if err != nil {
		return CommitToken{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "PUT", c.uploadEndpoint+"?upload_id="+uploadToken, r)
	if err != nil {
		return CommitToken{}, err
	}
	req.ContentLength = -1 // chunked transfer, matches gotohp
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Accept-Language", c.language)
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Authorization", "Bearer "+bearer)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return CommitToken{}, fmt.Errorf("gotohp: upload request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readBody(resp)
	if err != nil {
		return CommitToken{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return CommitToken{}, fmt.Errorf("gotohp: upload request failed with status %d: %s", resp.StatusCode, body)
	}
	return DecodeCommitToken(body)
}

// CommitUpload finalizes a prior UploadBytes call (step 3) and returns the
// permanent media key.
func (c *Client) CommitUpload(ctx context.Context, token CommitToken, fileName string, sha1Hash []byte, uploadTimestamp int64) (string, error) {
	model, deviceMk := c.deviceIdentity()
	quality := int64(3)
	if c.quality == QualityStorageSaver {
		quality = 1
	}
	body := EncodeCommitUpload(token, fileName, sha1Hash, uploadTimestamp, quality, model, deviceMk, androidAPIVersion)

	const maxRetries = 3
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(backoff(attempt - 1)):
			}
		}
		respBody, err := c.postProtobuf(ctx, c.commitEndpoint, body, true)
		if err == nil {
			return DecodeCommitUploadResponseMediaKey(respBody)
		}
		lastErr = err
	}
	return "", fmt.Errorf("gotohp: commit failed after %d attempts: %w", maxRetries+1, lastErr)
}

// FindRemoteMediaByHash looks up whether a file with this SHA1 already
// exists in the library, returning its media key if so (empty, nil if not
// found).
func (c *Client) FindRemoteMediaByHash(ctx context.Context, sha1Hash []byte) (string, error) {
	respBody, err := c.postProtobuf(ctx, c.hashCheckEndpoint, EncodeHashCheck(sha1Hash), false)
	if err != nil {
		return "", err
	}
	return DecodeRemoteMatchesMediaKey(respBody)
}

// CreateAlbum creates (or, per Google's API, effectively re-creates —
// gotohp always calls this to get-or-create) an album and returns its
// album media key.
func (c *Client) CreateAlbum(ctx context.Context, albumName string, mediaKeys []string) (string, error) {
	model, deviceMk := c.deviceIdentity()
	body := EncodeCreateAlbum(albumName, mediaKeys, time.Now().Unix(), model, deviceMk, androidAPIVersion)
	respBody, err := c.postProtobuf(ctx, c.createAlbumEndpoint, body, true)
	if err != nil {
		return "", err
	}
	return DecodeCreateAlbumResponseAlbumMediaKey(respBody)
}

// AddMediaToAlbum adds media items to an existing album by its album media
// key (for the "ExistingAlbum" path route).
func (c *Client) AddMediaToAlbum(ctx context.Context, albumMediaKey string, mediaKeys []string) error {
	model, deviceMk := c.deviceIdentity()
	body := EncodeAddMediaToAlbum(mediaKeys, albumMediaKey, time.Now().Unix(), model, deviceMk, androidAPIVersion)
	_, err := c.postProtobuf(ctx, c.addToAlbumEndpoint, body, true)
	return err
}
