package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
)

const (
	minSleep = 500 * time.Millisecond
	maxSleep = 10 * time.Second
	decay    = 2

	// MaxPageSize is the largest "limit" value e621 accepts per page of
	// /posts.json results.
	MaxPageSize = 320

	// userAgent identifies this backend (rather than the configured
	// e621 account) to e621's API, per https://e621.net/help/api's
	// requirement for a descriptive, contact-identifying User-Agent —
	// generic/default User-Agents get blocked.
	userAgent = "bclone / by benjithatfoxguy on e621 / dev@benjifox.gay"
)

// NewHTTPClient builds a plain http.Client for talking to e621. rclone's
// shared fs/fshttp client is deliberately not used here: its Transport
// unconditionally overwrites the User-Agent header on every request (see
// fs/fshttp.Transport.RoundTrip), which would stomp the descriptive,
// contact-identifying User-Agent e621's API requires (generic/default
// User-Agents get blocked — see https://e621.net/help/api).
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 10
	return &http.Client{Transport: transport, Timeout: 60 * time.Second}
}

// Client is a small wrapper around e621's (or an e621ng-compatible
// instance's) /posts.json endpoint.
type Client struct {
	srv   *rest.Client
	pacer *fs.Pacer
}

// NewClient builds a Client authenticated as username/apiKey against
// baseURL, e.g. "https://e621.net".
func NewClient(ctx context.Context, httpClient *http.Client, baseURL, username, apiKey string) *Client {
	srv := rest.NewClient(httpClient).SetRoot(baseURL)
	srv.SetUserPass(username, apiKey)
	srv.SetHeader("User-Agent", userAgent)
	return &Client{
		srv:   srv,
		pacer: fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(minSleep), pacer.MaxSleep(maxSleep), pacer.DecayConstant(decay))),
	}
}

// retryErrorCodes are HTTP statuses worth retrying.
var retryErrorCodes = []int{429, 500, 502, 503, 504}

func shouldRetry(ctx context.Context, res *http.Response, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}
	return fserrors.ShouldRetry(err) || fserrors.ShouldRetryHTTP(res, retryErrorCodes), err
}

// Posts fetches one page of results for the given tag search, newest
// first. page and limit follow e621's own pagination (limit is clamped to
// MaxPageSize by the API itself).
func (c *Client) Posts(ctx context.Context, tags string, page, limit int) ([]Post, error) {
	params := url.Values{}
	if tags != "" {
		params.Set("tags", tags)
	}
	params.Set("page", fmt.Sprintf("%d", page))
	params.Set("limit", fmt.Sprintf("%d", limit))

	var result PostsResponse
	opts := rest.Opts{
		Method:     "GET",
		Path:       "/posts.json",
		Parameters: params,
	}
	err := c.pacer.Call(func() (bool, error) {
		res, err := c.srv.CallJSON(ctx, &opts, nil, &result)
		return shouldRetry(ctx, res, err)
	})
	if err != nil {
		return nil, fmt.Errorf("e621: list posts: %w", err)
	}
	return result.Posts, nil
}

// PostByMD5 looks up the single post whose file has the given MD5, or
// returns fs.ErrorObjectNotFound if there is none.
func (c *Client) PostByMD5(ctx context.Context, md5 string) (*Post, error) {
	posts, err := c.Posts(ctx, "md5:"+md5, 1, 1)
	if err != nil {
		return nil, err
	}
	if len(posts) == 0 {
		return nil, fs.ErrorObjectNotFound
	}
	return &posts[0], nil
}

// CreatePost uploads a new file as a post via POST /uploads.json, sent as
// multipart/form-data under the "upload[file]" field with no tags,
// rating, or source attached — the target e621ng instance is expected to
// accept it (e.g. into a pending queue for review) without them.
//
// Unlike Posts/Fetch this isn't retried through the pacer: opts.Body is a
// single-pass reader over the upload content, and re-driving a partially
// consumed multipart body on retry would corrupt it.
func (c *Client) CreatePost(ctx context.Context, filename string, in io.Reader) error {
	opts := rest.Opts{
		Method:               "POST",
		Path:                 "/uploads.json",
		Body:                 in,
		MultipartParams:      url.Values{},
		MultipartContentName: "upload[file]",
		MultipartFileName:    filename,
	}
	if _, err := c.srv.CallJSON(ctx, &opts, nil, nil); err != nil {
		return fmt.Errorf("e621: upload post: %w", err)
	}
	return nil
}

// Fetch GETs an arbitrary absolute URL — a post's file lives on a
// separate CDN host from the API itself — and returns the streamed
// response body. Callers must Close it.
func (c *Client) Fetch(ctx context.Context, rawURL string, options []fs.OpenOption) (io.ReadCloser, error) {
	opts := rest.Opts{
		Method:  "GET",
		RootURL: rawURL,
		Options: options,
	}
	var res *http.Response
	err := c.pacer.Call(func() (bool, error) {
		var err error
		res, err = c.srv.Call(ctx, &opts)
		return shouldRetry(ctx, res, err)
	})
	if err != nil {
		return nil, fmt.Errorf("e621: fetch file: %w", err)
	}
	return res.Body, nil
}
