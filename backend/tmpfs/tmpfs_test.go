package tmpfs

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/rclone/rclone/backend/local"
	_ "github.com/rclone/rclone/backend/memory"
)

func TestDirectUseAllowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		_, err := fs.NewFs(context.Background(), ":tmpfs,remote=':memory:tmpfs-direct':")
		require.Error(t, err)
		assert.ErrorContains(t, err, "not supported on Windows")
		return
	}
	fsys, err := fs.NewFs(context.Background(), ":tmpfs,remote=':memory:tmpfs-direct',cleanup_on_shutdown=false:")
	require.NoError(t, err)
	require.NoError(t, fsys.(*Fs).Shutdown(context.Background()))
}

func TestMemoryRootMustNotBeEmpty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmpfs is not supported on Windows")
	}
	_, err := fs.NewFs(context.Background(), ":tmpfs,remote=':memory:':")
	require.ErrorContains(t, err, "non-empty dedicated root")
}

func TestValidateLocalRootRejectsBroadPaths(t *testing.T) {
	for _, root := range []string{"/", "/tmp", "/tmpfs", "/var", "/home", "/root", "/usr", "/etc", "/opt"} {
		t.Run(root, func(t *testing.T) {
			_, err := validateLocalRoot(root)
			require.Error(t, err)
		})
	}
}

func TestValidateLocalRootAllowsDedicatedChild(t *testing.T) {
	root := filepath.Join(t.TempDir(), "owned")
	got, err := validateLocalRoot(root)
	require.NoError(t, err)
	assert.Equal(t, filepath.Clean(root), got)
}

func TestValidateLocalRootResolvesSymlinks(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	require.NoError(t, osMkdirAll(target, 0o700))
	link := filepath.Join(parent, "link")
	require.NoError(t, osSymlink(target, link))

	got, err := validateLocalRoot(filepath.Join(link, "owned"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(target, "owned"), got)
}

func TestValidateLocalRootRejectsSymlinkToBroadRoot(t *testing.T) {
	parent := t.TempDir()
	link := filepath.Join(parent, "link")
	require.NoError(t, osSymlink("/", link))

	_, err := validateLocalRoot(link)
	require.Error(t, err)
}

func TestMissingLocalRootIsTreatedAsEmpty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmpfs is not supported on Windows")
	}
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "missing", "owned")

	fsys, err := fs.NewFs(ctx, ":tmpfs,remote='"+filepath.ToSlash(root)+"':")
	require.NoError(t, err)
	f := fsys.(*Fs)
	require.NoError(t, f.Shutdown(ctx))

	entries, err := osReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestMaxSizeRejectsOversizedWrites(t *testing.T) {
	f := newMemoryTmpfs(t, "max_size=5B,cleanup_on_shutdown=false")
	ctx := context.Background()

	require.NoError(t, putString(ctx, f, "a.txt", "12345", time.Now()))
	err := putString(ctx, f, "b.txt", "1", time.Now())
	require.ErrorContains(t, err, "would exceed max_size")

	require.NoError(t, putString(ctx, f, "a.txt", "1234", time.Now()))
	require.NoError(t, putString(ctx, f, "b.txt", "1", time.Now()))

	src := object.NewStaticObjectInfo("stream.txt", time.Now(), -1, true, nil, f)
	_, err = f.PutStream(ctx, strings.NewReader("x"), src)
	require.ErrorContains(t, err, "unknown-size")
}

func TestMaxAgeCleanup(t *testing.T) {
	f := newMemoryTmpfs(t, "max_age=1h,cleanup_on_shutdown=false")
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, putString(ctx, f, "old.txt", "old", now.Add(-2*time.Hour)))
	require.NoError(t, putString(ctx, f, "fresh.txt", "fresh", now))
	require.NoError(t, f.CleanUp(ctx))

	_, err := f.NewObject(ctx, "old.txt")
	assert.True(t, errors.Is(err, fs.ErrorObjectNotFound))
	_, err = f.NewObject(ctx, "fresh.txt")
	require.NoError(t, err)
}

func TestShutdownCleanupPreservesParentAndSibling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmpfs is not supported on Windows")
	}
	ctx := context.Background()
	parent := t.TempDir()
	root := filepath.Join(parent, "owned")
	sibling := filepath.Join(parent, "sibling")
	require.NoError(t, osMkdirAll(sibling, 0o700))
	require.NoError(t, osWriteFile(filepath.Join(sibling, "keep.txt"), []byte("keep"), 0o600))

	fsys, err := fs.NewFs(ctx, ":tmpfs,remote='"+filepath.ToSlash(root)+"',purge_on_start=false:")
	require.NoError(t, err)
	f := fsys.(*Fs)
	require.NoError(t, putString(ctx, f, "data.txt", "data", time.Now()))
	require.NoError(t, f.Shutdown(ctx))

	assert.FileExists(t, filepath.Join(sibling, "keep.txt"))
	entries, err := osReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func newMemoryTmpfs(t *testing.T, opts string) *Fs {
	t.Helper()
	bucket := strings.NewReplacer("/", "-", " ", "-", "_", "-").Replace(strings.ToLower(t.Name()))
	remote := ":tmpfs,remote=':memory:" + bucket + "'"
	if opts != "" {
		remote += "," + opts
	}
	remote += ":"
	fsys, err := fs.NewFs(context.Background(), remote)
	require.NoError(t, err)
	f := fsys.(*Fs)
	t.Cleanup(func() {
		_ = f.Shutdown(context.Background())
	})
	return f
}

func putString(ctx context.Context, f *Fs, remote, content string, modTime time.Time) error {
	src := object.NewStaticObjectInfo(remote, modTime, int64(len(content)), true, nil, f)
	_, err := f.Put(ctx, strings.NewReader(content), src)
	return err
}
