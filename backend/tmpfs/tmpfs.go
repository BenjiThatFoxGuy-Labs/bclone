// Package tmpfs implements an ephemeral policy adapter over another backend.
package tmpfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/cache"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fspath"
	"github.com/rclone/rclone/fs/list"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/lib/atexit"
)

const (
	defaultCleanupInterval = fs.Duration(time.Minute)
)

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "tmpfs",
		Description: "Ephemeral adapter for a dedicated temporary remote",
		NewFs:       NewFs,
		MetadataInfo: &fs.MetadataInfo{
			Help: `Any metadata supported by the underlying remote is read and written.`,
		},
		Options: []fs.Option{{
			Name:     "remote",
			Required: true,
			Help:     "Dedicated temporary remote to wrap.",
		}, {
			Name:     "max_size",
			Default:  fs.SizeSuffix(-1),
			Help:     "Maximum total size of data stored in the wrapped root.",
			Advanced: true,
		}, {
			Name:     "max_age",
			Default:  fs.DurationOff,
			Help:     "Expire objects older than this by modification time.",
			Advanced: true,
		}, {
			Name:     "cleanup_interval",
			Default:  defaultCleanupInterval,
			Help:     "How often to remove expired objects.",
			Advanced: true,
		}, {
			Name:     "cleanup_on_shutdown",
			Default:  true,
			Help:     "Purge the wrapped root when tmpfs shuts down.",
			Advanced: true,
		}, {
			Name:     "purge_on_start",
			Default:  true,
			Help:     "Purge the wrapped root when tmpfs starts.",
			Advanced: true,
		}},
	})
}

// Options defines the configuration for this backend.
type Options struct {
	Remote            string        `config:"remote"`
	MaxSize           fs.SizeSuffix `config:"max_size"`
	MaxAge            fs.Duration   `config:"max_age"`
	CleanupInterval   fs.Duration   `config:"cleanup_interval"`
	CleanupOnShutdown bool          `config:"cleanup_on_shutdown"`
	PurgeOnStart      bool          `config:"purge_on_start"`
}

// Fs represents a wrapped fs.Fs with ephemeral storage policy.
type Fs struct {
	fs.Fs
	name       string
	root       string
	wrapper    fs.Fs
	features   *fs.Features
	opt        Options
	localRoot  string
	rootID     string
	dirs       map[string]virtualDir
	dirsMu     sync.Mutex
	atexit     atexit.FnHandle
	cancel     context.CancelFunc
	shutdownMu sync.Mutex
	shutdown   bool
	quotaMu    sync.Mutex
}

type virtualDir struct {
	modTime time.Time
}

// Object wraps an object from the underlying backend.
type Object struct {
	fs.Object
	f *Fs
}

// NewFs constructs an Fs from the remote:path string.
func NewFs(ctx context.Context, name, rpath string, m configmap.Mapper) (fs.Fs, error) {
	if runtime.GOOS == "windows" {
		err := errors.New("tmpfs backend is not supported on Windows in v1")
		fs.Errorf(nil, "%v", err)
		return nil, err
	}

	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}
	if opt.Remote == "" {
		return nil, errors.New("tmpfs remote is required")
	}
	if strings.HasPrefix(opt.Remote, name+":") {
		return nil, errors.New("can't point tmpfs remote at itself")
	}

	remotePath := fspath.JoinRootPath(opt.Remote, rpath)
	baseFs, err := cache.Get(ctx, remotePath)
	if err != nil && err != fs.ErrorIsFile {
		return nil, fmt.Errorf("failed to create tmpfs backing remote %q: %w", opt.Remote, err)
	}

	f := &Fs{
		Fs:   baseFs,
		name: name,
		root: rpath,
		opt:  *opt,
		dirs: make(map[string]virtualDir),
	}
	if err == fs.ErrorIsFile {
		f.root = path.Dir(f.root)
		if f.root == "." || f.root == "/" {
			f.root = ""
		}
	}

	if err := f.validateBackingRoot(); err != nil {
		fs.Errorf(f, "%v", err)
		return nil, err
	}

	if err := f.ensureRoot(ctx); err != nil {
		return nil, err
	}

	f.features = (&fs.Features{
		CaseInsensitive:          true,
		DuplicateFiles:           true,
		ReadMimeType:             true,
		WriteMimeType:            true,
		CanHaveEmptyDirectories:  true,
		BucketBased:              true,
		BucketBasedRootOK:        true,
		SetTier:                  true,
		GetTier:                  true,
		ReadMetadata:             true,
		WriteMetadata:            true,
		UserMetadata:             true,
		ReadDirMetadata:          true,
		WriteDirMetadata:         true,
		WriteDirSetModTime:       true,
		UserDirMetadata:          true,
		DirModTimeUpdatesOnWrite: true,
		PartialUploads:           true,
	}).Fill(ctx, f).Mask(ctx, f.Fs).WrapsFs(f, f.Fs)
	f.features.CanHaveEmptyDirectories = true
	f.features.ListP = f.ListP

	if f.opt.PurgeOnStart {
		if err := f.purgeAll(ctx, "startup"); err != nil {
			return nil, err
		}
	}
	f.startCleaner(ctx)
	if f.opt.CleanupOnShutdown {
		f.atexit = atexit.Register(func() {
			_ = f.Shutdown(context.Background())
		})
	}
	cache.PinUntilFinalized(f.Fs, f)
	return f, err
}

// Name of the remote.
func (f *Fs) Name() string { return f.name }

// Root of the remote.
func (f *Fs) Root() string { return f.root }

// Features returns the optional features of this Fs.
func (f *Fs) Features() *fs.Features { return f.features }

// String converts this Fs to a string.
func (f *Fs) String() string {
	return fmt.Sprintf("tmpfs::%s:%s", f.name, f.root)
}

// UnWrap returns the Fs that this Fs is wrapping.
func (f *Fs) UnWrap() fs.Fs { return f.Fs }

// WrapFs returns the Fs that is wrapping this Fs.
func (f *Fs) WrapFs() fs.Fs { return f.wrapper }

// SetWrapper sets the Fs that is wrapping this Fs.
func (f *Fs) SetWrapper(wrapper fs.Fs) { f.wrapper = wrapper }

// List the objects and directories in dir into entries.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	return list.WithListP(ctx, dir, f)
}

// ListP lists objects and directories non-recursively.
func (f *Fs) ListP(ctx context.Context, dir string, callback fs.ListRCallback) error {
	wrappedCallback := func(entries fs.DirEntries) error {
		entries, err := f.wrapEntries(entries)
		if err != nil {
			return err
		}
		return callback(entries)
	}
	listP := f.Fs.Features().ListP
	if listP == nil {
		entries, err := f.Fs.List(ctx, dir)
		if err != nil {
			return err
		}
		return wrappedCallback(entries)
	}
	return listP(ctx, dir, wrappedCallback)
}

// ListR lists objects and directories recursively.
func (f *Fs) ListR(ctx context.Context, dir string, callback fs.ListRCallback) error {
	return f.Fs.Features().ListR(ctx, dir, func(entries fs.DirEntries) error {
		entries, err := f.wrapEntries(entries)
		if err != nil {
			return err
		}
		return callback(entries)
	})
}

// NewObject finds the Object at remote.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	o, err := f.Fs.NewObject(ctx, remote)
	return f.wrapObject(o, err)
}

// Put uploads an object.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	o, err := f.withQuota(ctx, src.Remote(), src.Size(), func() (fs.Object, error) {
		return f.Fs.Put(ctx, in, src, options...)
	})
	return f.wrapObject(o, err)
}

// PutStream uploads an object of unknown size when allowed.
func (f *Fs) PutStream(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	do := f.Fs.Features().PutStream
	if do == nil {
		return nil, errors.New("PutStream not supported by underlying remote")
	}
	o, err := f.withQuota(ctx, src.Remote(), src.Size(), func() (fs.Object, error) {
		return do(ctx, in, src, options...)
	})
	return f.wrapObject(o, err)
}

// PutUnchecked uploads an object, allowing duplicates if the wrapped remote supports it.
func (f *Fs) PutUnchecked(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	do := f.Fs.Features().PutUnchecked
	if do == nil {
		return nil, errors.New("PutUnchecked not supported by underlying remote")
	}
	o, err := f.withQuota(ctx, src.Remote(), src.Size(), func() (fs.Object, error) {
		return do(ctx, in, src, options...)
	})
	return f.wrapObject(o, err)
}

// Copy src to this remote using server-side copy operations.
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	do := f.Fs.Features().Copy
	if do == nil {
		return nil, fs.ErrorCantCopy
	}
	srcObj, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantCopy
	}
	o, err := f.withQuota(ctx, remote, src.Size(), func() (fs.Object, error) {
		return do(ctx, srcObj.Object, remote)
	})
	return f.wrapObject(o, err)
}

// Purge removes a directory and all of its contents inside the wrapped root.
func (f *Fs) Purge(ctx context.Context, dir string) error {
	if dir == "" {
		if err := f.validateCleanupRoot(); err != nil {
			return err
		}
	}
	return ignoreNotFound(operations.Purge(ctx, f.Fs, dir))
}

// CleanUp removes expired objects and delegates cleanup to the wrapped remote.
func (f *Fs) CleanUp(ctx context.Context) error {
	f.quotaMu.Lock()
	err := f.cleanExpiredLocked(ctx)
	f.quotaMu.Unlock()
	if err != nil {
		return err
	}
	if do := f.Fs.Features().CleanUp; do != nil {
		return do(ctx)
	}
	return nil
}

// About gets quota information from the Fs.
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	if f.maxSizeEnabled() {
		used, err := f.totalSize(ctx)
		if err != nil {
			return nil, err
		}
		free := int64(f.opt.MaxSize) - used
		if free < 0 {
			free = 0
		}
		return &fs.Usage{
			Total: fs.NewUsageValue(int64(f.opt.MaxSize)),
			Used:  fs.NewUsageValue(used),
			Free:  fs.NewUsageValue(free),
		}, nil
	}
	if do := f.Fs.Features().About; do != nil {
		return do(ctx)
	}
	used, err := f.totalSize(ctx)
	if err != nil {
		return nil, err
	}
	return &fs.Usage{Used: fs.NewUsageValue(used)}, nil
}

// DirSetModTime sets the directory modtime for dir.
func (f *Fs) DirSetModTime(ctx context.Context, dir string, modTime time.Time) error {
	if do := f.Fs.Features().DirSetModTime; do != nil {
		return do(ctx, dir, modTime)
	}
	return fs.ErrorNotImplemented
}

// MkdirMetadata makes a directory with metadata.
func (f *Fs) MkdirMetadata(ctx context.Context, dir string, metadata fs.Metadata) (fs.Directory, error) {
	if do := f.Fs.Features().MkdirMetadata; do != nil {
		return do(ctx, dir, metadata)
	}
	return nil, fs.ErrorNotImplemented
}

// PublicLink generates a public link to the remote path.
func (f *Fs) PublicLink(ctx context.Context, remote string, expire fs.Duration, unlink bool) (string, error) {
	if do := f.Fs.Features().PublicLink; do != nil {
		return do(ctx, remote, expire, unlink)
	}
	return "", fs.ErrorNotImplemented
}

// UserInfo returns info about the connected user.
func (f *Fs) UserInfo(ctx context.Context) (map[string]string, error) {
	if do := f.Fs.Features().UserInfo; do != nil {
		return do(ctx)
	}
	return nil, fs.ErrorNotImplemented
}

// Disconnect the current user.
func (f *Fs) Disconnect(ctx context.Context) error {
	if do := f.Fs.Features().Disconnect; do != nil {
		return do(ctx)
	}
	return fs.ErrorNotImplemented
}

// Shutdown stops background work and optionally purges the wrapped root.
func (f *Fs) Shutdown(ctx context.Context) error {
	f.shutdownMu.Lock()
	if f.shutdown {
		f.shutdownMu.Unlock()
		return nil
	}
	f.shutdown = true
	cancel := f.cancel
	cleanup := f.opt.CleanupOnShutdown
	handle := f.atexit
	f.shutdownMu.Unlock()

	if handle != nil {
		atexit.Unregister(handle)
	}
	if cancel != nil {
		cancel()
	}

	var err error
	if cleanup {
		err = f.purgeAll(ctx, "shutdown")
	}
	if do := f.Fs.Features().Shutdown; do != nil {
		if err2 := do(ctx); err == nil {
			err = err2
		}
	}
	return err
}

func (f *Fs) startCleaner(ctx context.Context) {
	if !f.maxAgeEnabled() || f.opt.CleanupInterval <= 0 || f.opt.CleanupInterval == fs.DurationOff {
		return
	}
	cleanCtx, cancel := context.WithCancel(context.Background())
	cleanCtx = fs.CopyConfig(cleanCtx, ctx)
	f.cancel = cancel
	go func() {
		ticker := time.NewTicker(time.Duration(f.opt.CleanupInterval))
		defer ticker.Stop()
		for {
			select {
			case <-cleanCtx.Done():
				return
			case <-ticker.C:
				if err := f.CleanUp(cleanCtx); err != nil {
					fs.Errorf(f, "cleanup failed: %v", err)
				}
			}
		}
	}()
}

func (f *Fs) wrapEntries(baseEntries fs.DirEntries) (entries fs.DirEntries, err error) {
	entries = baseEntries[:0]
	for _, entry := range baseEntries {
		if obj, ok := entry.(fs.Object); ok {
			wrapped, err := f.wrapObject(obj, nil)
			if err != nil {
				return nil, err
			}
			entries = append(entries, wrapped)
		} else {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func (f *Fs) wrapObject(o fs.Object, err error) (fs.Object, error) {
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, fs.ErrorObjectNotFound
	}
	return &Object{Object: o, f: f}, nil
}

func (f *Fs) withQuota(ctx context.Context, remote string, newSize int64, write func() (fs.Object, error)) (fs.Object, error) {
	if !f.maxSizeEnabled() {
		return write()
	}
	if newSize < 0 {
		return nil, fmt.Errorf("tmpfs: can't write unknown-size object %q when max_size is set", remote)
	}

	f.quotaMu.Lock()
	defer f.quotaMu.Unlock()

	if err := f.cleanExpiredLocked(ctx); err != nil {
		return nil, err
	}
	current, err := f.totalSizeLocked(ctx)
	if err != nil {
		return nil, err
	}
	existing, err := f.sizeOfExisting(ctx, remote)
	if err != nil {
		return nil, err
	}
	after := current - existing + newSize
	if after > int64(f.opt.MaxSize) {
		return nil, fmt.Errorf("tmpfs: write of %q would exceed max_size %s (current %s, existing %s, new %s)",
			remote, f.opt.MaxSize, fs.SizeSuffix(current), fs.SizeSuffix(existing), fs.SizeSuffix(newSize))
	}
	return write()
}

func (f *Fs) totalSize(ctx context.Context) (int64, error) {
	f.quotaMu.Lock()
	defer f.quotaMu.Unlock()
	return f.totalSizeLocked(ctx)
}

func (f *Fs) totalSizeLocked(ctx context.Context) (total int64, err error) {
	err = operations.ListFn(ctx, f.Fs, func(o fs.Object) {
		total += o.Size()
	})
	if errors.Is(err, fs.ErrorDirNotFound) {
		err = nil
	}
	return total, err
}

func (f *Fs) sizeOfExisting(ctx context.Context, remote string) (int64, error) {
	o, err := f.Fs.NewObject(ctx, remote)
	if errors.Is(err, fs.ErrorObjectNotFound) || errors.Is(err, fs.ErrorDirNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return o.Size(), nil
}

func (f *Fs) cleanExpiredLocked(ctx context.Context) error {
	if !f.maxAgeEnabled() {
		return nil
	}
	if err := f.validateCleanupRoot(); err != nil {
		return err
	}
	cutoff := time.Now().Add(-time.Duration(f.opt.MaxAge))
	var expired []fs.Object
	err := operations.ListFn(ctx, f.Fs, func(o fs.Object) {
		if o.ModTime(ctx).Before(cutoff) {
			expired = append(expired, o)
		}
	})
	if errors.Is(err, fs.ErrorDirNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, o := range expired {
		if err := o.Remove(ctx); err != nil && !errors.Is(err, fs.ErrorObjectNotFound) {
			return err
		}
	}
	return ignoreNotFound(operations.Rmdirs(ctx, f.Fs, "", true))
}

func (f *Fs) purgeAll(ctx context.Context, why string) error {
	f.quotaMu.Lock()
	defer f.quotaMu.Unlock()
	if err := f.validateCleanupRoot(); err != nil {
		return err
	}
	fs.Infof(f, "Purging tmpfs backing root on %s", why)
	if err := ignoreNotFound(operations.Purge(ctx, f.Fs, "")); err != nil {
		return err
	}
	// Ensure the root directory exists after purging.
	if err := f.Fs.Mkdir(ctx, ""); err != nil {
		// Ignore error if the directory already exists.
		if _, err2 := f.Fs.Stat(ctx, ""); err2 == nil {
			return nil
		}
		return fmt.Errorf("failed to create tmpfs backing root after purge: %w", err)
	}
	return nil
}

// ensureRoot makes sure the root directory exists in the wrapped remote.
func (f *Fs) ensureRoot(ctx context.Context) error {
	if err := f.Fs.Mkdir(ctx, ""); err != nil {
		// Ignore error if the directory already exists.
		if _, err2 := f.Fs.Stat(ctx, ""); err2 == nil {
			return nil
		}
		return fmt.Errorf("failed to create tmpfs backing root: %w", err)
	}
	return nil
}

func (f *Fs) maxSizeEnabled() bool {
	return f.opt.MaxSize >= 0
}

func (f *Fs) maxAgeEnabled() bool {
	return f.opt.MaxAge > 0 && f.opt.MaxAge != fs.DurationOff
}

func ignoreNotFound(err error) error {
	if errors.Is(err, fs.ErrorDirNotFound) || errors.Is(err, fs.ErrorObjectNotFound) || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (f *Fs) validateBackingRoot() error {
	if f.Fs.Features().IsLocal {
		root, err := validateLocalRoot(f.Fs.Root())
		if err != nil {
			return err
		}
		f.localRoot = root
		f.rootID = root
		return nil
	}
	root := strings.Trim(f.Fs.Root(), "/")
	if root == "" || root == "." {
		return fmt.Errorf("tmpfs remote %q must point at a non-empty dedicated root", fs.ConfigString(f.Fs))
	}
	f.rootID = f.Fs.Root()
	return nil
}

func (f *Fs) validateCleanupRoot() error {
	if f.Fs.Features().IsLocal {
		root, err := validateLocalRoot(f.Fs.Root())
		if err != nil {
			return err
		}
		if root != f.localRoot {
			return fmt.Errorf("tmpfs local root changed from %q to %q; refusing cleanup", f.localRoot, root)
		}
		return nil
	}
	root := strings.Trim(f.Fs.Root(), "/")
	if root == "" || root == "." || f.Fs.Root() != f.rootID {
		return fmt.Errorf("tmpfs remote root changed or is unsafe; refusing cleanup")
	}
	return nil
}

func validateLocalRoot(root string) (string, error) {
	if root == "" {
		return "", errors.New("tmpfs local remote must not be empty")
	}
	native := filepath.Clean(filepath.FromSlash(root))
	if !filepath.IsAbs(native) {
		return "", fmt.Errorf("tmpfs local remote %q must be absolute", root)
	}
	canonical, err := canonicalLocalPath(native)
	if err != nil {
		return "", err
	}
	if isBroadLocalRoot(canonical) {
		return "", fmt.Errorf("tmpfs local remote %q resolves to broad path %q", root, canonical)
	}
	if pathDepth(canonical) < 2 {
		return "", fmt.Errorf("tmpfs local remote %q must be a dedicated child path", root)
	}
	return canonical, nil
}

func canonicalLocalPath(name string) (string, error) {
	cleaned := filepath.Clean(name)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return filepath.Clean(resolved), nil
	}

	current := cleaned
	var missing []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("tmpfs local remote %q has no existing parent", name)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func isBroadLocalRoot(name string) bool {
	known := map[string]struct{}{
		filepath.Clean("/"):                       {},
		filepath.Clean("/tmp"):                    {},
		filepath.Clean("/private/tmp"):            {},
		filepath.Clean("/tmpfs"):                  {},
		filepath.Clean("/var"):                    {},
		filepath.Clean("/private/var"):            {},
		filepath.Clean("/var/tmp"):                {},
		filepath.Clean("/private/var/tmp"):        {},
		filepath.Clean("/home"):                   {},
		filepath.Clean("/root"):                   {},
		filepath.Clean("/Users"):                  {},
		filepath.Clean("/usr"):                    {},
		filepath.Clean("/etc"):                    {},
		filepath.Clean("/opt"):                    {},
		filepath.Clean("/Volumes"):                {},
		filepath.Clean("/var/lib"):                {},
		filepath.Clean("/var/lib/docker"):         {},
		filepath.Clean("/var/lib/docker/volumes"): {},
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		known[filepath.Clean(home)] = struct{}{}
	}
	_, found := known[filepath.Clean(name)]
	return found
}

func pathDepth(name string) int {
	volume := filepath.VolumeName(name)
	name = strings.TrimPrefix(name, volume)
	name = strings.Trim(name, string(filepath.Separator))
	if name == "" {
		return 0
	}
	return len(strings.Split(name, string(filepath.Separator)))
}

// Fs returns read only access to the Fs that this object is part of.
func (o *Object) Fs() fs.Info { return o.f }

// UnWrap returns the wrapped Object.
func (o *Object) UnWrap() fs.Object { return o.Object }

// Update the object with the given data, time and size.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	_, err := o.f.withQuota(ctx, src.Remote(), src.Size(), func() (fs.Object, error) {
		return o.Object, o.Object.Update(ctx, in, src, options...)
	})
	return err
}

// ID returns the ID of the Object if possible.
func (o *Object) ID() string {
	if doer, ok := o.Object.(fs.IDer); ok {
		return doer.ID()
	}
	return ""
}

// GetTier returns the Tier of the Object if possible.
func (o *Object) GetTier() string {
	if doer, ok := o.Object.(fs.GetTierer); ok {
		return doer.GetTier()
	}
	return ""
}

// SetTier sets the Tier of the Object if possible.
func (o *Object) SetTier(tier string) error {
	if doer, ok := o.Object.(fs.SetTierer); ok {
		return doer.SetTier(tier)
	}
	return fs.ErrorNotImplemented
}

// MimeType of an Object if known, "" otherwise.
func (o *Object) MimeType(ctx context.Context) string {
	if doer, ok := o.Object.(fs.MimeTyper); ok {
		return doer.MimeType(ctx)
	}
	return ""
}

// Metadata returns metadata for an object.
func (o *Object) Metadata(ctx context.Context) (fs.Metadata, error) {
	do, ok := o.Object.(fs.Metadataer)
	if !ok {
		return nil, nil
	}
	return do.Metadata(ctx)
}

// SetMetadata sets metadata for an Object.
func (o *Object) SetMetadata(ctx context.Context, metadata fs.Metadata) error {
	do, ok := o.Object.(fs.SetMetadataer)
	if !ok {
		return fs.ErrorNotImplemented
	}
	return do.SetMetadata(ctx, metadata)
}

// Check interfaces are satisfied.
var (
	_ fs.Fs              = (*Fs)(nil)
	_ fs.Purger          = (*Fs)(nil)
	_ fs.Copier          = (*Fs)(nil)
	_ fs.PutUncheckeder  = (*Fs)(nil)
	_ fs.PutStreamer     = (*Fs)(nil)
	_ fs.CleanUpper      = (*Fs)(nil)
	_ fs.UnWrapper       = (*Fs)(nil)
	_ fs.ListRer         = (*Fs)(nil)
	_ fs.ListPer         = (*Fs)(nil)
	_ fs.Abouter         = (*Fs)(nil)
	_ fs.Wrapper         = (*Fs)(nil)
	_ fs.DirSetModTimer  = (*Fs)(nil)
	_ fs.MkdirMetadataer = (*Fs)(nil)
	_ fs.PublicLinker    = (*Fs)(nil)
	_ fs.UserInfoer      = (*Fs)(nil)
	_ fs.Disconnecter    = (*Fs)(nil)
	_ fs.Shutdowner      = (*Fs)(nil)
	_ fs.FullObject      = (*Object)(nil)
	_ fs.MimeTyper       = (*Object)(nil)
	_ fs.ObjectUnWrapper = (*Object)(nil)
)
