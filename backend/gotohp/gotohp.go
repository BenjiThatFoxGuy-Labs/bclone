// Package gotohp implements a write-only backend for the unofficial Google
// Photos native (Android app) upload protocol, reimplemented from
// https://github.com/xob0t/gotohp (MIT licensed) — see backend/gotohp/api
// for protocol details and why gotohp's own Go code isn't imported
// directly.
package gotohp

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/backend/gotohp/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/hash"
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "gotohp",
		Description: "Google Photos (unofficial, write-only upload API)",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name:      "auth",
			Sensitive: true,
			Required:  true,
			Help: `Auth credential captured from the Google Photos Android app.

This backend only supports the non-rooted credential flow: install a
patched/ReVanced Google Photos APK, sign in, and capture the credential via
'adb logcat | grep "auth%2Fphotos.native"' — see
https://github.com/xob0t/gotohp#authentication for the full walkthrough.
Paste the full "androidId=...&app=...&client_sig=...&Email=...&..." string.

Accounts that require rooted-device token binding are not supported.`,
		}, {
			Name:     "album",
			Advanced: true,
			Help: `Pin all uploads through this remote to one album (created if needed).

When set, remote paths are just flat object names — every upload goes into
this one album regardless of path.

When unset (the default), paths are used to route uploads instead:

    gotohp:NewAlbum/<AlbumName>/<file>       create-or-get album <AlbumName>
    gotohp:ExistingAlbum/<AlbumID>/<file>    add to album by its literal ID
    gotohp:<file>                            upload loose, no album`,
		}, {
			Name:     "quality",
			Advanced: true,
			Default:  "original",
			Examples: []fs.OptionExample{{
				Value: "original",
				Help:  "Upload in original quality (counts against account storage quota).",
			}, {
				Value: "storage_saver",
				Help:  "Upload in storage saver quality (may be compressed by Google).",
			}},
			Help: "Upload quality tier.",
		}, {
			Name:     "use_quota",
			Advanced: true,
			Default:  false,
			Help:     "Force uploads to count against account storage quota (overrides quality's device spoof).",
		}, {
			Name:     "skip_existing",
			Advanced: true,
			Default:  true,
			Help: `Skip uploading files that already exist in the library (by content hash).

Since this remote can't list or read back the library, rclone's usual
skip-if-exists logic can't run — this performs the equivalent check
per-file against Google's hash index before uploading.`,
		}, {
			Name:     "phantom_ttl",
			Advanced: true,
			Default:  fs.Duration(5 * time.Minute),
			Help: `How long a just-uploaded file keeps appearing in listings/stats/reads.

Google Photos has no read-back API, so this remote can't normally answer
"does this file exist" or "give me its bytes" after upload. To avoid mount
or serve clients that verify a write by immediately statting or reading it
back (and spuriously retrying when they get ENOENT), recently-uploaded
files are kept visible — including their real content, for Open() — in a
local cache for this long after upload. Set this to at least your
--dir-cache-time / --vfs-cache-max-age so the illusion holds for as long as
a mount's own cache would anyway.`,
		}, {
			Name:     "settle_time",
			Advanced: true,
			Default:  fs.Duration(4 * time.Second),
			Help: `How long to wait after a write before actually uploading to Google.

Some clients (notably many SFTP/WebDAV clients via 'rclone serve') write to
a temporary name and then rename to the final name. Without a settle delay
this remote would upload the temporary file too. Uploads are deferred by
this long and cancelled/superseded if the file is overwritten, renamed
away from, or removed before the timer fires. This requires VFS caching
(--vfs-cache-mode writes or full) to be effective end-to-end for mount/serve.`,
		}},
	})
}

// Options defines the configuration for this backend.
type Options struct {
	Auth         string      `config:"auth"`
	Album        string      `config:"album"`
	Quality      string      `config:"quality"`
	UseQuota     bool        `config:"use_quota"`
	SkipExisting bool        `config:"skip_existing"`
	PhantomTTL   fs.Duration `config:"phantom_ttl"`
	SettleTime   fs.Duration `config:"settle_time"`
}

// albumMode identifies how a resolved path should be associated with an album.
type albumMode int

const (
	albumNone albumMode = iota
	albumNew
	albumExisting
)

// resolvedPath is the result of routing a logical remote path.
type resolvedPath struct {
	mode     albumMode
	albumRef string
	leaf     string
}

// phantomEntry is a locally-cached record of a recently-Put object: enough
// to satisfy List/NewObject/Open for a while after upload without any
// remote read-back capability.
type phantomEntry struct {
	size      int64
	modTime   time.Time
	sha1      []byte
	mediaKey  string // filled in once the deferred upload actually commits
	localPath string // spooled bytes backing Open(); "" once swept
	createdAt time.Time
}

// pendingUpload is a Put() awaiting its settle_time before the real Google
// upload dance fires.
type pendingUpload struct {
	remote    string
	localPath string
	sha1      []byte
	size      int64
	modTime   time.Time
	fileName  string
	mode      albumMode
	albumRef  string
	timer     *time.Timer
}

// Fs represents a gotohp remote.
type Fs struct {
	name     string
	root     string
	opt      Options
	features *fs.Features
	client   *api.Client

	albumMu sync.Mutex
	albums  map[string]string // album name -> album media key, "NewAlbum"/config-album create-or-get cache

	phantomDir string
	phantomMu  sync.Mutex
	phantom    map[string]*phantomEntry // key: remote path relative to f.root

	pendingMu sync.Mutex
	pending   map[string]*pendingUpload // key: remote path relative to f.root
}

// NewFs constructs a new Fs from the path, container:path
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}

	quality := api.QualityOriginal
	if opt.Quality == "storage_saver" {
		quality = api.QualityStorageSaver
	}
	client, err := api.NewClient(api.NewHTTPClient(), opt.Auth, quality, opt.UseQuota)
	if err != nil {
		return nil, err
	}

	spoolDir, err := os.MkdirTemp("", "gotohp-spool-*")
	if err != nil {
		return nil, fmt.Errorf("gotohp: failed to create local spool directory: %w", err)
	}

	f := &Fs{
		name:       name,
		root:       strings.Trim(root, "/"),
		opt:        *opt,
		client:     client,
		albums:     map[string]string{},
		phantomDir: spoolDir,
		phantom:    map[string]*phantomEntry{},
		pending:    map[string]*pendingUpload{},
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
func (f *Fs) String() string { return fmt.Sprintf("gotohp root '%s'", f.root) }

// Precision of the remote: Google Photos doesn't let us set an arbitrary modtime.
func (f *Fs) Precision() time.Duration { return fs.ModTimeNotSupported }

// Hashes returns the supported hash types.
func (f *Fs) Hashes() hash.Set { return hash.NewHashSet(hash.SHA1) }

// Features returns the optional features of this Fs.
func (f *Fs) Features() *fs.Features { return f.features }

// resolvePath routes a remote (relative to f.root) to an album target and
// leaf filename, per the path scheme documented on the "album" option.
func (f *Fs) resolvePath(remote string) resolvedPath {
	if f.opt.Album != "" {
		return resolvedPath{mode: albumNew, albumRef: f.opt.Album, leaf: remote}
	}
	full := remote
	if f.root != "" {
		if full == "" {
			full = f.root
		} else {
			full = f.root + "/" + full
		}
	}
	parts := strings.SplitN(full, "/", 3)
	if len(parts) == 3 {
		switch parts[0] {
		case "NewAlbum":
			return resolvedPath{mode: albumNew, albumRef: parts[1], leaf: parts[2]}
		case "ExistingAlbum":
			return resolvedPath{mode: albumExisting, albumRef: parts[1], leaf: parts[2]}
		}
	}
	return resolvedPath{mode: albumNone, leaf: full}
}

// --- phantom cache ---

func (f *Fs) setPhantom(remote string, entry *phantomEntry) {
	f.phantomMu.Lock()
	if old, ok := f.phantom[remote]; ok && old.localPath != "" && old.localPath != entry.localPath {
		_ = os.Remove(old.localPath)
	}
	f.phantom[remote] = entry
	f.phantomMu.Unlock()
}

func (f *Fs) getPhantom(remote string) (*phantomEntry, bool) {
	f.phantomMu.Lock()
	defer f.phantomMu.Unlock()
	e, ok := f.phantom[remote]
	return e, ok
}

func (f *Fs) removePhantom(remote string) {
	f.phantomMu.Lock()
	if old, ok := f.phantom[remote]; ok {
		if old.localPath != "" {
			_ = os.Remove(old.localPath)
		}
		delete(f.phantom, remote)
	}
	f.phantomMu.Unlock()
}

func (f *Fs) sweepPhantom() {
	ttl := time.Duration(f.opt.PhantomTTL)
	cutoff := time.Now().Add(-ttl)
	f.phantomMu.Lock()
	for remote, entry := range f.phantom {
		if entry.createdAt.Before(cutoff) {
			if entry.localPath != "" {
				_ = os.Remove(entry.localPath)
			}
			delete(f.phantom, remote)
		}
	}
	f.phantomMu.Unlock()
}

// List returns recently-uploaded (still-lingering) entries under dir; this
// remote has no real remote listing capability.
func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	f.sweepPhantom()
	f.phantomMu.Lock()
	defer f.phantomMu.Unlock()

	prefix := dir
	if prefix != "" {
		prefix += "/"
	}
	var entries fs.DirEntries
	seenDirs := map[string]bool{}
	for remote, entry := range f.phantom {
		if dir != "" && !strings.HasPrefix(remote, prefix) {
			continue
		}
		rest := strings.TrimPrefix(remote, prefix)
		if rest == "" {
			continue
		}
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			dirPath := prefix + rest[:slash]
			if !seenDirs[dirPath] {
				seenDirs[dirPath] = true
				entries = append(entries, fs.NewDir(dirPath, entry.modTime))
			}
			continue
		}
		entries = append(entries, f.newObject(remote, entry))
	}
	return entries, nil
}

// NewObject finds an object recently uploaded under this remote, if it's
// still within its lingering window.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	f.sweepPhantom()
	entry, ok := f.getPhantom(remote)
	if !ok {
		return nil, fs.ErrorObjectNotFound
	}
	return f.newObject(remote, entry), nil
}

func (f *Fs) newObject(remote string, entry *phantomEntry) *Object {
	return &Object{
		f:       f,
		remote:  remote,
		size:    entry.size,
		modTime: entry.modTime,
		sha1:    entry.sha1,
	}
}

// Mkdir is a no-op: albums are created lazily by Put via the NewAlbum path route.
func (f *Fs) Mkdir(ctx context.Context, dir string) error { return nil }

// Rmdir is a no-op: Google's API has no album deletion, and directories
// here are synthetic.
func (f *Fs) Rmdir(ctx context.Context, dir string) error { return nil }

// Put spools in to a local temp file, makes the write immediately visible
// via the phantom cache, and schedules the real Google upload after
// settle_time (see the "settle_time" option).
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	if src.Size() == 0 {
		return nil, fs.ErrorCantUploadEmptyFiles
	}
	remote := src.Remote()

	localPath, sha1Sum, size, err := f.spool(in)
	if err != nil {
		return nil, err
	}

	modTime := src.ModTime(ctx)
	entry := &phantomEntry{
		size:      size,
		modTime:   modTime,
		sha1:      sha1Sum,
		localPath: localPath,
		createdAt: time.Now(),
	}
	f.setPhantom(remote, entry)

	rp := f.resolvePath(remote)
	pu := &pendingUpload{
		remote:    remote,
		localPath: localPath,
		sha1:      sha1Sum,
		size:      size,
		modTime:   modTime,
		fileName:  path.Base(rp.leaf),
		mode:      rp.mode,
		albumRef:  rp.albumRef,
	}
	f.schedulePending(remote, pu)

	return f.newObject(remote, entry), nil
}

func (f *Fs) spool(in io.Reader) (localPath string, sha1Sum []byte, size int64, err error) {
	tmp, err := os.CreateTemp(f.phantomDir, "upload-*")
	if err != nil {
		return "", nil, 0, fmt.Errorf("gotohp: failed to create spool file: %w", err)
	}
	defer func() { _ = tmp.Close() }()

	h := sha1.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), in)
	if err != nil {
		_ = os.Remove(tmp.Name())
		return "", nil, 0, fmt.Errorf("gotohp: failed to spool upload: %w", err)
	}
	return tmp.Name(), h.Sum(nil), n, nil
}

// --- settle-time pending-upload debounce ---

func (f *Fs) schedulePending(remote string, pu *pendingUpload) {
	f.pendingMu.Lock()
	if old, ok := f.pending[remote]; ok {
		old.timer.Stop()
	}
	f.pending[remote] = pu
	delay := time.Duration(f.opt.SettleTime)
	pu.timer = time.AfterFunc(delay, func() {
		f.firePending(remote, pu)
	})
	f.pendingMu.Unlock()
}

func (f *Fs) firePending(remote string, pu *pendingUpload) {
	f.pendingMu.Lock()
	current, ok := f.pending[remote]
	isCurrent := ok && current == pu
	if isCurrent {
		delete(f.pending, remote)
	}
	f.pendingMu.Unlock()
	if !isCurrent {
		// Superseded by a newer write, or cancelled by Remove.
		return
	}
	f.commitPending(context.Background(), pu)
}

func (f *Fs) commitPending(ctx context.Context, pu *pendingUpload) {
	mediaKey, err := f.uploadToGoogle(ctx, pu)
	if err != nil {
		fs.Errorf(f, "gotohp: deferred upload of %q failed: %v", pu.remote, err)
		return
	}
	f.phantomMu.Lock()
	if entry, ok := f.phantom[pu.remote]; ok && entry.localPath == pu.localPath {
		entry.mediaKey = mediaKey
	}
	f.phantomMu.Unlock()
}

// uploadToGoogle runs the real dedup-check / 3-step upload / album-add
// sequence. The 3 upload steps (get token, upload bytes, commit) must all
// complete together or the uploaded bytes become an orphaned upload that
// never becomes a photo.
func (f *Fs) uploadToGoogle(ctx context.Context, pu *pendingUpload) (string, error) {
	var mediaKey string
	if f.opt.SkipExisting {
		existing, err := f.client.FindRemoteMediaByHash(ctx, pu.sha1)
		if err != nil {
			fs.Debugf(f, "gotohp: hash lookup failed, proceeding with upload: %v", err)
		} else if existing != "" {
			mediaKey = existing
		}
	}

	if mediaKey == "" {
		uploadToken, err := f.client.GetUploadToken(ctx, pu.sha1, pu.size)
		if err != nil {
			return "", fmt.Errorf("get upload token: %w", err)
		}
		commitToken, err := f.client.UploadBytes(ctx, uploadToken, func() (io.ReadCloser, error) {
			return os.Open(pu.localPath)
		})
		if err != nil {
			return "", fmt.Errorf("upload bytes: %w", err)
		}
		mediaKey, err = f.client.CommitUpload(ctx, commitToken, pu.fileName, pu.sha1, pu.modTime.Unix())
		if err != nil {
			return "", fmt.Errorf("commit upload: %w", err)
		}
	}

	if pu.mode != albumNone && pu.albumRef != "" {
		if err := f.addToAlbum(ctx, pu.mode, pu.albumRef, mediaKey); err != nil {
			return mediaKey, fmt.Errorf("add to album %q: %w", pu.albumRef, err)
		}
	}
	return mediaKey, nil
}

// addToAlbum resolves ref to an album media key and adds mediaKey to it.
// Google's API has no create-empty-album call, so for a brand new
// "NewAlbum" name the very first item is created together with the album
// (matching gotohp's own createNewAlbum); subsequent items for the same
// name are added to the now-cached album.
func (f *Fs) addToAlbum(ctx context.Context, mode albumMode, ref string, mediaKey string) error {
	if mode == albumExisting {
		return f.client.AddMediaToAlbum(ctx, ref, []string{mediaKey})
	}
	f.albumMu.Lock()
	id, ok := f.albums[ref]
	if !ok {
		var err error
		id, err = f.client.CreateAlbum(ctx, ref, []string{mediaKey})
		if err != nil {
			f.albumMu.Unlock()
			return err
		}
		f.albums[ref] = id
		f.albumMu.Unlock()
		return nil
	}
	f.albumMu.Unlock()
	return f.client.AddMediaToAlbum(ctx, id, []string{mediaKey})
}

// removeObject implements Object.Remove: if the upload hasn't been
// committed to Google yet (still within settle_time), it's cancelled
// locally at no network cost; otherwise deletion isn't possible (Google's
// upload API has no remote-delete call).
func (f *Fs) removeObject(remote string) error {
	f.pendingMu.Lock()
	pu, stillPending := f.pending[remote]
	if stillPending {
		delete(f.pending, remote)
	}
	f.pendingMu.Unlock()

	if stillPending {
		pu.timer.Stop()
		f.removePhantom(remote)
		return nil
	}

	if _, known := f.getPhantom(remote); known {
		return errors.New("gotohp: remote delete is not supported by the Google Photos upload API")
	}
	return fs.ErrorObjectNotFound
}

// Shutdown flushes any uploads still waiting out their settle_time so nothing
// is silently lost on a clean unmount/exit, then cleans up the spool directory.
func (f *Fs) Shutdown(ctx context.Context) error {
	f.pendingMu.Lock()
	pending := make([]*pendingUpload, 0, len(f.pending))
	for _, pu := range f.pending {
		pu.timer.Stop()
		pending = append(pending, pu)
	}
	f.pending = map[string]*pendingUpload{}
	f.pendingMu.Unlock()

	var wg sync.WaitGroup
	for _, pu := range pending {
		wg.Add(1)
		go func(pu *pendingUpload) {
			defer wg.Done()
			f.commitPending(ctx, pu)
		}(pu)
	}
	wg.Wait()

	_ = os.RemoveAll(f.phantomDir)
	return nil
}

// Object describes a gotohp object backed by the phantom cache.
type Object struct {
	f       *Fs
	remote  string
	size    int64
	modTime time.Time
	sha1    []byte
}

// Fs returns the parent Fs.
func (o *Object) Fs() fs.Info { return o.f }

// String returns the remote path.
func (o *Object) String() string { return o.remote }

// Remote returns the remote path.
func (o *Object) Remote() string { return o.remote }

// ModTime returns the modification time as supplied at upload time.
func (o *Object) ModTime(ctx context.Context) time.Time { return o.modTime }

// Size returns the size as supplied at upload time.
func (o *Object) Size() int64 { return o.size }

// Storable returns whether this object can be stored.
func (o *Object) Storable() bool { return true }

// SetModTime is not supported: Google Photos doesn't expose an API for it here.
func (o *Object) SetModTime(ctx context.Context, t time.Time) error {
	return fs.ErrorCantSetModTime
}

// Hash returns the SHA1 computed at upload time.
func (o *Object) Hash(ctx context.Context, ht hash.Type) (string, error) {
	if ht != hash.SHA1 {
		return "", hash.ErrUnsupported
	}
	return hex.EncodeToString(o.sha1), nil
}

// readCloser pairs an independent Reader and Closer.
type readCloser struct {
	io.Reader
	io.Closer
}

// Open serves the locally-spooled bytes for a recent upload, if still
// within its lingering window (see the "phantom_ttl" option). This is a
// write-only remote: reading is only possible during that window.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	entry, ok := o.f.getPhantom(o.remote)
	if !ok || entry.localPath == "" {
		return nil, errors.New("gotohp: this is a write-only remote; reading is only possible briefly after upload, and that window has passed")
	}
	file, err := os.Open(entry.localPath)
	if err != nil {
		return nil, fmt.Errorf("gotohp: cached upload no longer available: %w", err)
	}

	var offset, limit int64 = 0, -1
	for _, opt := range options {
		switch x := opt.(type) {
		case *fs.SeekOption:
			offset = x.Offset
		case *fs.RangeOption:
			offset, limit = x.Decode(o.size)
		}
	}
	if offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	if limit >= 0 {
		return readCloser{Reader: io.LimitReader(file, limit), Closer: file}, nil
	}
	return file, nil
}

// Update replaces this object's content, following the same spool +
// phantom-cache + settle_time path as Put.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	newObj, err := o.f.Put(ctx, in, src, options...)
	if err != nil {
		return err
	}
	*o = *(newObj.(*Object))
	return nil
}

// Remove cancels a still-pending (not yet committed) upload locally, or
// fails: Google's upload API has no remote-delete call for already
// committed media.
func (o *Object) Remove(ctx context.Context) error {
	return o.f.removeObject(o.remote)
}

// Check interface satisfaction.
var (
	_ fs.Fs         = (*Fs)(nil)
	_ fs.Shutdowner = (*Fs)(nil)
	_ fs.Object     = (*Object)(nil)
)
