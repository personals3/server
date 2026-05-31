// Package storage implements the on-disk object layout.
//
// Layout:
//
//	{root}/buckets/{bucket-uuid}/objects/{sha256(key)}/data
//	{root}/buckets/{bucket-uuid}/tmp/{upload-id}/part-NNNN     (multipart)
//	{root}/segments/{object-uuid}/...                          (transcoder output)
package storage

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type FS struct {
	root string
}

func New(root string) (*FS, error) {
	for _, sub := range []string{"buckets", "segments"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", sub, err)
		}
	}
	return &FS{root: root}, nil
}

// Root returns the absolute path the FS was initialized with — used for
// disk-stat queries on the filesystem holding the storage.
func (f *FS) Root() string { return f.root }

// HashKey produces the deterministic directory name for an object key.
// Two different keys never collide (SHA-256). Same key always maps to same path.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// ObjectPath returns the absolute path to an object's data file.
func (f *FS) ObjectPath(bucketID, key string) string {
	return filepath.Join(f.root, "buckets", bucketID, "objects", HashKey(key), "data")
}

// BucketDir returns the per-bucket root.
func (f *FS) BucketDir(bucketID string) string {
	return filepath.Join(f.root, "buckets", bucketID)
}

// SegmentsDir returns the per-object directory used by the transcoder
// for HLS playlists, segments, thumbnails, and image renditions.
func (f *FS) SegmentsDir(objectID string) string {
	return filepath.Join(f.root, "segments", objectID)
}

// MultipartPartPath returns the path for one part of an in-progress upload.
func (f *FS) MultipartPartPath(bucketID, uploadID string, partNumber int) string {
	return filepath.Join(f.root, "buckets", bucketID, "tmp", uploadID, fmt.Sprintf("part-%05d", partNumber))
}

// MultipartDir returns the per-upload tmp directory.
func (f *FS) MultipartDir(bucketID, uploadID string) string {
	return filepath.Join(f.root, "buckets", bucketID, "tmp", uploadID)
}

// MkdirAllMultipart ensures the per-upload tmp directory exists. Idempotent.
// Called by the presigned-multipart initiator so that part PUTs from
// browsers don't have to race on a missing parent.
func (f *FS) MkdirAllMultipart(bucketID, uploadID string) error {
	return os.MkdirAll(f.MultipartDir(bucketID, uploadID), 0o755)
}

// RemoveAbandonedUpload deletes the tmp directory for a multipart upload.
// Used when an upload is aborted (manual cancel, or post-refresh cleanup).
func (f *FS) RemoveAbandonedUpload(bucketID, uploadID string) error {
	return os.RemoveAll(f.MultipartDir(bucketID, uploadID))
}

// CreateBucketDir creates the directory tree for a new bucket.
func (f *FS) CreateBucketDir(bucketID string) error {
	for _, sub := range []string{"objects", "tmp"} {
		if err := os.MkdirAll(filepath.Join(f.BucketDir(bucketID), sub), 0o755); err != nil {
			return fmt.Errorf("create bucket dir: %w", err)
		}
	}
	return nil
}

// RemoveBucketDir deletes a bucket's entire data directory.
func (f *FS) RemoveBucketDir(bucketID string) error {
	return os.RemoveAll(f.BucketDir(bucketID))
}

// WriteObject streams r to disk, computing MD5 as it goes.
// Returns (bytesWritten, etag) on success.
//
// Atomic write: data goes to a .tmp file first, then renamed into place —
// avoids leaving partial objects on disk if the write fails mid-stream.
func (f *FS) WriteObject(bucketID, key string, r io.Reader) (int64, string, error) {
	finalPath := f.ObjectPath(bucketID, key)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return 0, "", fmt.Errorf("mkdir: %w", err)
	}

	tmpPath := finalPath + ".tmp"
	tmp, err := os.Create(tmpPath)
	if err != nil {
		return 0, "", fmt.Errorf("create tmp: %w", err)
	}

	hasher := md5.New()
	mw := io.MultiWriter(tmp, hasher)

	n, err := io.Copy(mw, r)
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return 0, "", fmt.Errorf("copy: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return 0, "", fmt.Errorf("sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return 0, "", fmt.Errorf("close: %w", err)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return 0, "", fmt.Errorf("rename: %w", err)
	}

	etag := hex.EncodeToString(hasher.Sum(nil))
	return n, etag, nil
}

// OpenObject opens an object for streaming download.
// Caller must close the returned file.
func (f *FS) OpenObject(bucketID, key string) (*os.File, error) {
	return os.Open(f.ObjectPath(bucketID, key))
}

// StatObject returns the FileInfo for an object.
func (f *FS) StatObject(bucketID, key string) (os.FileInfo, error) {
	return os.Stat(f.ObjectPath(bucketID, key))
}

// RemoveObject deletes both the data file and the parent hash directory.
// NOTE: this wipes the whole hash dir, including any `versions/` snapshots.
// Use RemoveCurrentOnly() when versioning is on and you only want to drop
// the current data file but keep prior versions.
func (f *FS) RemoveObject(bucketID, key string) error {
	dir := filepath.Dir(f.ObjectPath(bucketID, key))
	return os.RemoveAll(dir)
}

// RemoveCurrentOnly removes just the `data` file (the current version),
// leaving the versions/ subdirectory intact. Used when the object is being
// deleted in a versioning-on bucket.
func (f *FS) RemoveCurrentOnly(bucketID, key string) error {
	err := os.Remove(f.ObjectPath(bucketID, key))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// -----------------------------------------------------------------------------
// Versioning helpers
// -----------------------------------------------------------------------------

// VersionPath returns the absolute path of a specific version of an object,
// stored next to the current `data` file:
//
//	buckets/{bucketID}/objects/{sha256(key)}/versions/{versionId}
func (f *FS) VersionPath(bucketID, key, versionID string) string {
	return filepath.Join(filepath.Dir(f.ObjectPath(bucketID, key)),
		"versions", versionID)
}

// SnapshotCurrent atomically renames the current `data` file aside into the
// versions/ directory under the given versionID. Returns the new absolute path.
// Caller is responsible for inserting the matching object_versions DB row.
func (f *FS) SnapshotCurrent(bucketID, key, versionID string) (string, error) {
	src := f.ObjectPath(bucketID, key)
	dst := f.VersionPath(bucketID, key, versionID)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("mkdir versions: %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		return "", fmt.Errorf("snapshot rename: %w", err)
	}
	return dst, nil
}

// OpenVersion opens a specific version's bytes for streaming download.
func (f *FS) OpenVersion(bucketID, key, versionID string) (*os.File, error) {
	return os.Open(f.VersionPath(bucketID, key, versionID))
}

// RemoveVersion deletes a specific version's file from disk. Used for
// purging a single version without touching the rest.
func (f *FS) RemoveVersion(bucketID, key, versionID string) error {
	err := os.Remove(f.VersionPath(bucketID, key, versionID))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// PromoteVersion COPIES a stored version back into the current `data` slot,
// re-computing its MD5 along the way. The version itself stays on disk so it
// can still be restored later. Returns (size, etag).
//
// Caller MUST have already snapshotted whatever was at `data` before — this
// will overwrite it.
func (f *FS) PromoteVersion(bucketID, key, versionID string) (int64, string, error) {
	src := f.VersionPath(bucketID, key, versionID)
	dst := f.ObjectPath(bucketID, key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, "", fmt.Errorf("mkdir: %w", err)
	}

	in, err := os.Open(src)
	if err != nil {
		return 0, "", fmt.Errorf("open version: %w", err)
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, "", fmt.Errorf("create tmp: %w", err)
	}

	hasher := md5.New()
	mw := io.MultiWriter(out, hasher)
	n, err := io.Copy(mw, in)
	if err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return 0, "", fmt.Errorf("copy: %w", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return 0, "", fmt.Errorf("sync: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, "", fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return 0, "", fmt.Errorf("rename: %w", err)
	}

	etag := hex.EncodeToString(hasher.Sum(nil))
	return n, etag, nil
}
