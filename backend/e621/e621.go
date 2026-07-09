// Package e621 provides a filesystem view over e621 (and e621ng-compatible
// instances), backed by the site's public JSON API.
//
// e621 has no folder tree of its own, so this backend exposes a small,
// fixed set of virtual directories instead of a real one:
//
//	e621:favorites (alias: favs)   posts tagged fav:me
//	e621:recent                    the most recent posts
//	e621:me                        posts tagged user:me (uploaded by the configured account)
//	e621:search/<tags>             posts matching the arbitrary tag search <tags>
//
// Every object, in any of the above or fetched directly, is named
// "<md5>.<ext>" — e621's own content hash and file extension. A bare
// "<md5>.<ext>" can also be fetched directly (e.g. `rclone copyto
// e621:<md5>.<ext> ./out.jpg`) by searching md5:<md5>, even outside the
// directories above and without listing it first — but only for direct
// one-shot commands (copy, copyto, cat, ...), not while running under
// mount/serve. See directFetch below for why. "search/<tags>" is similarly
// left out of the root listing (there's no fixed set of queries to
// enumerate) but reachable the same way, and additionally usable as the
// root of a mount/union/serve pointed directly at "e621:search/<tags>".
//
// Uploading is supported, but only by writing into the "upload" directory
// at the root (e621:upload/<file>) — no other path is writable, even when
// uploading is otherwise available. It's refused against the official
// e621.net, e926.net and e6ai.net instances, and can additionally be
// turned off for any other endpoint with the disable_upload option. When
// unavailable either way, the "upload" directory itself doesn't exist: it
// won't appear listing the root, and nothing can be written to it.
package e621

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/rclone/rclone/backend/e621/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/vfs/vfscommon"
)

const defaultEndpoint = "https://e621.net"

// officialHosts are the instances uploading is refused against — see the
// package doc comment.
var officialHosts = map[string]bool{
	"e621.net": true,
	"e926.net": true,
	"e6ai.net": true,
}

// isOfficialInstance reports whether endpoint (a base URL like
// "https://e621.net") points at one of the official e621 instances.
func isOfficialInstance(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	return officialHosts[strings.ToLower(strings.TrimPrefix(u.Hostname(), "www."))]
}

// Names of the fixed virtual directories this backend exposes, and the
// e621 tag search each one corresponds to.
const (
	dirFavorites = "favorites"
	dirFavs      = "favs"
	dirRecent    = "recent"
	dirMe        = "me"

	// dirUpload is the sole writable directory (e621:upload/<file>); see
	// Fs.uploadsAllowed and Fs.Put. It only exists in the root listing
	// (and can only be written to) when uploading is available.
	dirUpload = "upload"

	// dirSearch is a parametrized virtual directory: "search/<tags>" lists
	// posts matching the arbitrary e621 tag search <tags> (the same
	// space-separated syntax e621's own search box takes — path segments
	// beneath "search/" are never percent-decoded, so a literal space in
	// the path is enough, no manual URL-encoding needed). It's left out of
	// the root listing below since there's no fixed set of queries to
	// enumerate, so it never shows up browsing that way; it still works
	// for direct fetches (copy/copyto/cat/...), and shows up under
	// mount/union/serve if one of those is pointed directly at
	// "e621:search/<tags>" as its root.
	dirSearch = "search"
)

var virtualDirs = map[string]string{
	dirFavorites: "fav:me",
	dirFavs:      "fav:me",
	dirRecent:    "",
	dirMe:        "user:me",
}

// resolveTags returns the e621 tag search a (root-joined) virtual
// directory path corresponds to, and whether it names one at all.
func resolveTags(full string) (tags string, ok bool) {
	if tags, ok := virtualDirs[full]; ok {
		return tags, true
	}
	if rest, found := strings.CutPrefix(full, dirSearch+"/"); found && rest != "" {
		return rest, true
	}
	return "", false
}

// md5Leaf matches a bare content-addressed object name, e.g. "<32 hex
// chars>.jpg".
var md5Leaf = regexp.MustCompile(`^([0-9a-fA-F]{32})\.([A-Za-z0-9]+)$`)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "e621",
		Description: "e621 (API-based booru browser; upload support for non-official e621ng instances)",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name:     "username",
			Help:     "e621 username.",
			Required: true,
		}, {
			Name:      "api_key",
			Help:      "e621 API key.\n\nGenerate one from \"My Account\" -> \"Manage API Access\" on e621.",
			Required:  true,
			Sensitive: true,
		}, {
			Name:     "endpoint",
			Default:  defaultEndpoint,
			Advanced: true,
			Help: fmt.Sprintf(`The API endpoint to use.

Override this to target a different e621ng-compatible instance (for
example e926.net, or a self-hosted e621ng deployment) instead of the
default %s. This is arbitrary input: the backend makes a best effort to
talk to whatever endpoint is given, but only e621ng-compatible instances
are supported.

Uploading only works against a non-official instance: it's always refused
for e621.net, e926.net and e6ai.net, regardless of this setting (see
disable_upload to also turn it off elsewhere).`, defaultEndpoint),
		}, {
			Name:     "disable_upload",
			Default:  false,
			Advanced: true,
			Help: `Disable uploading even against a non-official, otherwise upload-capable endpoint.

Uploading is always refused against the official e621.net, e926.net and
e6ai.net instances regardless of this setting; set this to additionally
disable it for any other endpoint. Either way, when uploading is
unavailable the "upload" directory itself doesn't exist: it's left out of
the root listing, and nothing can be written to it.`,
		}, {
			Name:     "list_max",
			Default:  1000,
			Advanced: true,
			Help: `Maximum number of posts to consider when listing the favorites, favs, recent or me directories.

e621 paginates its post listings in pages of up to 320; listing walks
pages until this many posts have been seen. Raise it to see further back
into a large list, at the cost of more API calls per listing.`,
		}},
	})
}

// Options defines the configuration for this backend.
type Options struct {
	Username      string `config:"username"`
	APIKey        string `config:"api_key"`
	Endpoint      string `config:"endpoint"`
	DisableUpload bool   `config:"disable_upload"`
	ListMax       int    `config:"list_max"`
}

// Fs represents an e621 remote.
type Fs struct {
	name     string
	root     string
	opt      Options
	features *fs.Features
	client   *api.Client

	// directFetch allows NewObject to resolve a bare "<md5>.<ext>" outside
	// the known virtual directories by searching md5:<md5> directly. It's
	// meant for one-shot commands (copy, copyto, cat, ...) and is switched
	// off when running under mount/serve, so that arbitrary stat() calls
	// made while browsing a mount (shell globbing, thumbnailers, etc.)
	// can't turn into e621 API searches for paths that were never listed.
	//
	// Detection reuses the same heuristic as the gotohp backend
	// (vfscommon.Opt.CacheMode): rclone gives backends no direct signal for
	// "am I being driven by a VFS layer", only the global --vfs-cache-mode
	// flag that mount/serve populate. A mount/serve run left at the
	// default --vfs-cache-mode off won't be detected here and will still
	// allow direct fetches; that's an accepted limitation, not a bug.
	directFetch bool

	// uploadsAllowed is false when opt.Endpoint resolves to one of the
	// official e621.net/e926.net/e6ai.net instances (see officialHosts) or
	// opt.DisableUpload is set. When false, the "upload" directory is
	// hidden from List and unwritable; when true, it's the *only*
	// writable directory (see Put).
	uploadsAllowed bool
}

// NewFs constructs a new Fs from the path, container:path.
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(opt.Endpoint, "/")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}

	f := &Fs{
		name:           name,
		root:           strings.Trim(root, "/"),
		opt:            *opt,
		client:         api.NewClient(ctx, api.NewHTTPClient(), endpoint, opt.Username, opt.APIKey),
		directFetch:    vfscommon.Opt.CacheMode == vfscommon.CacheModeOff,
		uploadsAllowed: !isOfficialInstance(endpoint) && !opt.DisableUpload,
	}
	f.features = (&fs.Features{
		CanHaveEmptyDirectories: false,
		NoMultiThreading:        true,
	}).Fill(ctx, f)
	return f, nil
}

// Name of the remote.
func (f *Fs) Name() string { return f.name }

// Root of the remote.
func (f *Fs) Root() string { return f.root }

// String converts this Fs to a string.
func (f *Fs) String() string { return fmt.Sprintf("e621 root '%s'", f.root) }

// Precision of the remote: we have no way of knowing e621's, so estimate 1s.
func (f *Fs) Precision() time.Duration { return time.Second }

// Hashes returns the supported hash types.
func (f *Fs) Hashes() hash.Set { return hash.Set(hash.MD5) }

// Features returns the optional features of this Fs.
func (f *Fs) Features() *fs.Features { return f.features }

// List returns the fixed virtual directories at the root, or the posts
// matching one of their tag searches.
func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	full := path.Join(f.root, dir)
	if full == "" {
		names := []string{dirFavorites, dirFavs, dirRecent, dirMe}
		if f.uploadsAllowed {
			names = append(names, dirUpload)
		}
		entries := make(fs.DirEntries, 0, len(names))
		for _, name := range names {
			entries = append(entries, fs.NewDir(path.Join(dir, name), time.Time{}))
		}
		return entries, nil
	}

	if full == dirUpload {
		if !f.uploadsAllowed {
			return nil, fs.ErrorDirNotFound
		}
		// Write-only: uploaded posts aren't tracked, so there's nothing to
		// list back here.
		return fs.DirEntries{}, nil
	}

	tags, ok := resolveTags(full)
	if !ok {
		return nil, fs.ErrorDirNotFound
	}

	var entries fs.DirEntries
	seen := 0
	for page := 1; ; page++ {
		posts, err := f.client.Posts(ctx, tags, page, api.MaxPageSize)
		if err != nil {
			return nil, err
		}
		for _, p := range posts {
			seen++
			if p.File.MD5 == "" || p.File.URL == "" {
				continue // post's file isn't accessible; nothing to list
			}
			remote := path.Join(dir, p.File.MD5+"."+p.File.Ext)
			entries = append(entries, f.newObject(remote, p))
		}
		if len(posts) < api.MaxPageSize || seen >= f.opt.ListMax {
			break
		}
	}
	return entries, nil
}

// NewObject finds the object at remote, a bare "<md5>.<ext>", by searching
// md5:<md5> against the API. See directFetch for when this is allowed
// outside the known virtual directories.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	full := path.Join(f.root, remote)
	dir := path.Dir(full)
	if dir == "." {
		dir = ""
	}
	leaf := path.Base(full)

	m := md5Leaf.FindStringSubmatch(leaf)
	if m == nil {
		return nil, fs.ErrorObjectNotFound
	}
	if _, known := resolveTags(dir); !known && !f.directFetch {
		return nil, fs.ErrorObjectNotFound
	}

	post, err := f.client.PostByMD5(ctx, m[1])
	if err != nil {
		return nil, err
	}
	if post.File.MD5 == "" || post.File.URL == "" {
		return nil, fmt.Errorf("e621: post %d has no accessible file (deleted or restricted)", post.ID)
	}
	return f.newObject(remote, *post), nil
}

func (f *Fs) newObject(remote string, p api.Post) *Object {
	modTime, _ := time.Parse(time.RFC3339, p.CreatedAt)
	return &Object{
		f:       f,
		remote:  remote,
		id:      p.ID,
		size:    p.File.Size,
		modTime: modTime,
		md5:     p.File.MD5,
		url:     p.File.URL,
	}
}

// Mkdir is a no-op: this backend's directories are all synthetic.
func (f *Fs) Mkdir(ctx context.Context, dir string) error { return nil }

// Rmdir is not supported: e621's API has no post-deletion call available
// to this backend.
func (f *Fs) Rmdir(ctx context.Context, dir string) error { return fs.ErrorPermissionDenied }

// Put uploads a new post via /uploads.json. Only "upload/<file>" is
// writable — see dirUpload — and only when uploading is available at all
// — see uploadsAllowed.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	if !f.uploadsAllowed {
		return nil, fmt.Errorf("e621: uploading is disabled (official e621.net/e926.net/e6ai.net instance, or disable_upload is set): %w", fs.ErrorPermissionDenied)
	}
	full := path.Join(f.root, src.Remote())
	dir := path.Dir(full)
	if dir == "." {
		dir = ""
	}
	if dir != dirUpload {
		return nil, fmt.Errorf("e621: only the %q directory is writable: %w", dirUpload, fs.ErrorPermissionDenied)
	}
	if src.Size() == 0 {
		return nil, fs.ErrorCantUploadEmptyFiles
	}

	hasher := md5.New()
	filename := path.Base(src.Remote())
	if err := f.client.CreatePost(ctx, filename, io.TeeReader(in, hasher)); err != nil {
		return nil, err
	}

	// e621ng's /uploads.json response shape isn't guaranteed across
	// instances, so rather than parse it for the resulting post/md5, use
	// the hash computed while streaming the upload directly: it's exactly
	// what the created post will be keyed by. The file's URL is left
	// unset; Open on this Object will only work once the post is
	// resolved afresh via NewObject/List (e.g. after it clears any
	// pending-post queue).
	return &Object{
		f:       f,
		remote:  src.Remote(),
		size:    src.Size(),
		modTime: src.ModTime(ctx),
		md5:     hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

// Object describes an e621 post's file.
type Object struct {
	f       *Fs
	remote  string
	id      int64
	size    int64
	modTime time.Time
	md5     string
	url     string
}

// Fs returns the parent Fs.
func (o *Object) Fs() fs.Info { return o.f }

// String returns the remote path.
func (o *Object) String() string { return o.remote }

// Remote returns the remote path.
func (o *Object) Remote() string { return o.remote }

// ModTime returns the post's creation time.
func (o *Object) ModTime(ctx context.Context) time.Time { return o.modTime }

// Size returns the file size in bytes.
func (o *Object) Size() int64 { return o.size }

// Storable returns whether this object can be stored.
func (o *Object) Storable() bool { return true }

// SetModTime is not supported: e621 posts' timestamps aren't editable
// through this backend.
func (o *Object) SetModTime(ctx context.Context, t time.Time) error {
	return fs.ErrorPermissionDenied
}

// Hash returns the MD5 hash e621 identifies this file's content by.
func (o *Object) Hash(ctx context.Context, ht hash.Type) (string, error) {
	if ht != hash.MD5 {
		return "", hash.ErrUnsupported
	}
	return o.md5, nil
}

// Open the file for reading, straight from e621's file CDN.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	if o.url == "" {
		return nil, errors.New("e621: this post's file isn't accessible (deleted or restricted)")
	}
	fs.FixRangeOption(options, o.size)
	return o.f.client.Fetch(ctx, o.url, options)
}

// Update uploads a new post with this content, following the same rules
// as Fs.Put (there's no "replace an existing post" call to use instead).
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	newObj, err := o.f.Put(ctx, in, src, options...)
	if err != nil {
		return err
	}
	*o = *(newObj.(*Object))
	return nil
}

// Remove is not supported: e621's API has no post-deletion call available
// to this backend.
func (o *Object) Remove(ctx context.Context) error { return fs.ErrorPermissionDenied }

// Check interface satisfaction.
var (
	_ fs.Fs     = (*Fs)(nil)
	_ fs.Object = (*Object)(nil)
)
