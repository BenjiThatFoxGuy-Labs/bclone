---
title: "Google Photos (gotohp)"
description: "Rclone docs for the write-only gotohp Google Photos backend"
versionIntroduced: "v1.70"
---

# {{< icon "fas fa-images fa-fw" >}} Google Photos (gotohp)

`gotohp` uploads files to Google Photos using the unofficial Android-app
upload protocol, reimplemented from
[xob0t/gotohp](https://github.com/xob0t/gotohp) (MIT licensed). It is a
**write-only** remote: Google's native upload API has no download,
remote-delete, or general listing capability, so this backend can only
`Put`/`copy`/`move` files *to* Google Photos, never list, read back, or
delete what's already there.

This is unrelated to, and does not replace, the existing `googlephotos`
backend, which uses Google's official (OAuth) Photos Library API and
supports full read/list/album management. `gotohp` exists as an
alternative upload path with different tradeoffs by presenting as the
official Android app instead of using the official REST API.

Because it impersonates a specific Android device/app, `gotohp` is
inherently a reverse-engineered, unofficial integration. It could break if
Google changes the protocol, and using it carries some risk to the
account (rate limiting, flags) that an official API integration would not.
Use it with that understanding.

## Authentication

`gotohp` does not use OAuth. It replays a credential captured from a real
(or emulated) Android device running a patched Google Photos client:

1. Install a patched/[ReVanced](https://revanced.app/) Google Photos APK
   on a device or emulator and sign in with the target account.
2. Connect via `adb` and run:
   ```sh
   adb logcat | grep "auth%2Fphotos.native"
   ```
3. Copy the resulting `androidId=...&app=...&client_sig=...&Email=...&...`
   line — that whole string is the `auth` config value.

See [gotohp's own authentication docs](https://github.com/xob0t/gotohp#authentication)
for the full walkthrough. Only this non-rooted flow is supported here —
accounts that require the rooted-device token-binding variant (extracting
a binding key via Android's AccountManager) are not supported and will
fail with a clear error rather than being silently mishandled.

Treat this credential like a password: it's stored with `Sensitive: true`
so `rclone config redacted` and logs redact it, but anyone who has it can
upload to that Google account as the Photos app.

## Configuration

```sh
rclone config
```

```text
Storage> gotohp
Auth credential captured from the Google Photos Android app.
auth> androidId=XXXX&app=com.google.android.apps.photos&client_sig=XXXX&Email=you@gmail.com&Token=XXXX&lang=en_US&service=oauth2:https://www.googleapis.com/auth/photos.native
```

Everything else is an advanced option with a sensible default — see
below.

## Path scheme: albums

Google Photos has no folder tree, just a flat library plus albums. Remote
paths route uploads instead of naming real directories:

```text
gotohp:NewAlbum/<AlbumName>/<file>       create-or-get album <AlbumName>, upload <file> into it
gotohp:ExistingAlbum/<AlbumID>/<file>    add <file> to the album with this literal album ID
gotohp:<file>                            upload loose, no album
```

If the `album` option is set, every upload through that remote goes into
that one (create-or-get) album regardless of path, and paths are just flat
object names — the `NewAlbum`/`ExistingAlbum` routing above only applies
when `album` is unset.

`Mkdir`/`Rmdir` are no-ops: albums are created lazily by the first upload
that targets them, and Google's API has no album-deletion call.

## Two modes: synchronous (plain commands) vs. deferred (mount/serve)

This backend automatically behaves differently depending on how it's
being used — there is no config option to set:

- **Plain commands** (`copy`/`sync`/`move`/...): every `Put` uploads to
  Google **synchronously** — the call doesn't return until the file has
  actually landed in the library. The process uploads each file in turn
  and exits as soon as it's done, with no leftover background work.
- **`rclone mount` / `rclone serve` / `rclone nfsmount`** (Docker
  included), run with `--vfs-cache-mode writes` or `full`: a different
  mode switches on automatically, where two problems show up that don't
  matter for a one-shot `copy`:
  1. This remote can't list or read back the library, so immediately
     after a `Put`, a `stat` or re-read of that same path would normally
     get "not found" — which is exactly the kind of thing that makes a
     mount client, or a client copying over `rclone serve`, think the
     write failed and retry it. In this mode, a just-uploaded file's
     metadata **and actual bytes** are kept in a local cache and served
     back for a while after upload, via `List`, `NewObject`, and `Open`.
  2. Some clients (many SFTP/WebDAV clients driven through
     `rclone serve`) write to a temporary filename and then rename to
     the final name. Uploading the temporary file to Google would be
     wasteful and would litter the library, so in this mode the actual
     upload to Google is deferred for a while after each write, and
     cancelled/superseded if that path is overwritten, renamed away
     from, or removed before the timer fires — only the final write for
     a given path actually reaches Google.

Both the defer delay and the post-upload lingering window reuse rclone's
own `--vfs-write-back` duration (default 5s) — the same setting you're
already tuning for this exact purpose — so there's nothing gotohp-specific
to configure. Detection itself is based on the global `--vfs-cache-mode`
flag: it's how rclone tells backends "VFS is buffering your writes
locally," which is also the actual precondition for deferring uploads to
be correct, so a plain `copy`/`sync`/`move` (which never touches that
flag) always gets the synchronous behavior with zero chance of an
unwanted delay. A clean `Shutdown` (unmount, or serve exiting normally)
flushes any uploads still waiting out their defer delay rather than
dropping them.

The one edge case this doesn't cover: a single rclone process (via
`rclone rcd` or `librclone`) running multiple mounts/serves at once with
*different* `--vfs-cache-mode` settings, since the flag is process-wide,
not per-mount. This doesn't affect the common case of one `mount`/`serve`
per process (e.g. one per Docker container).

Google Photos also rejects (silently drops) uploaded bytes that don't
look like a real supported photo/video, so test uploads must use a real
image or video file, not arbitrary bytes.

## Standard options

Here are the standard options specific to gotohp (Google Photos (unofficial, write-only upload API)).

### --gotohp-auth

Auth credential captured from the Google Photos Android app.

This backend only supports the non-rooted credential flow: install a
patched/ReVanced Google Photos APK, sign in, and capture the credential via
`adb logcat | grep "auth%2Fphotos.native"` — see
https://github.com/xob0t/gotohp#authentication for the full walkthrough.
Paste the full `androidId=...&app=...&client_sig=...&Email=...&...` string.

Accounts that require rooted-device token binding are not supported.

Properties:

- Config:      auth
- Env Var:     RCLONE_GOTOHP_AUTH
- Type:        string
- Required:    true

## Advanced options

Here are the advanced options specific to gotohp (Google Photos (unofficial, write-only upload API)).

### --gotohp-album

Pin all uploads through this remote to one album (created if needed).

When set, remote paths are just flat object names — every upload goes
into this one album regardless of path. When unset (the default), paths
route uploads instead, per the path scheme above.

Properties:

- Config:      album
- Env Var:     RCLONE_GOTOHP_ALBUM
- Type:        string
- Required:    false

### --gotohp-quality

Upload quality tier.

Properties:

- Config:      quality
- Env Var:     RCLONE_GOTOHP_QUALITY
- Type:        string
- Default:     "original"
- Examples:
    - "original"
        - Upload in original quality (counts against account storage quota).
    - "storage_saver"
        - Upload in storage saver quality (may be compressed by Google).

### --gotohp-use-quota

Force uploads to count against account storage quota (overrides quality's device spoof).

Properties:

- Config:      use_quota
- Env Var:     RCLONE_GOTOHP_USE_QUOTA
- Type:        bool
- Default:     false

### --gotohp-skip-existing

Skip uploading files that already exist in the library (by content hash).

Since this remote can't list or read back the library, rclone's usual
skip-if-exists logic can't run — this performs the equivalent check
per-file against Google's hash index before uploading.

Properties:

- Config:      skip_existing
- Env Var:     RCLONE_GOTOHP_SKIP_EXISTING
- Type:        bool
- Default:     true

## Limitations

- Write-only: no download, no remote delete, no real listing (only a
  short-lived local cache of recent uploads — see above).
- No modtime support (`SetModTime` always fails); the modtime supplied at
  upload time is kept locally for as long as the phantom cache entry
  lives, but isn't stored by Google.
- Album deletion is not supported (Google's API doesn't offer it).
- Rooted-device token-binding accounts are not supported.
