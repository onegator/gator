package conversations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// DiskStore keeps files under a directory on the server.
type DiskStore struct{ Root string }

// Put implements BlobStore. The file appears whole or not at all.
func (d DiskStore) Put(_ context.Context, key string, r io.Reader, size int64, _ string) error {
	path, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%w: the file ended after %d of %d bytes", ErrInvalid, n, size)
	}
	return os.Rename(tmp.Name(), path)
}

// Open implements BlobStore.
func (d DiskStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	path, err := d.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoBlob
	}
	return f, err
}

// Delete implements BlobStore.
func (d DiskStore) Delete(_ context.Context, key string) error {
	path, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// path keeps every key inside Root, whatever it says.
func (d DiskStore) path(key string) (string, error) {
	path := filepath.Join(d.Root, filepath.FromSlash(key))
	if rel, err := filepath.Rel(d.Root, path); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("storage key %q leaves the attachments directory", key)
	}
	return path, nil
}

// S3Store keeps files in an S3-compatible bucket.
type S3Store struct {
	client *minio.Client
	bucket string
	prefix string
}

// S3Config names the bucket.
type S3Config struct {
	Endpoint, Bucket, Prefix, AccessKey, SecretKey string
	UseSSL                                         bool
}

// NewS3Store connects to the bucket.
func NewS3Store(c S3Config) (*S3Store, error) {
	client, err := minio.New(c.Endpoint, &minio.Options{Creds: credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""), Secure: c.UseSSL})
	if err != nil {
		return nil, err
	}
	return &S3Store{client: client, bucket: c.Bucket, prefix: strings.Trim(c.Prefix, "/")}, nil
}

func (s *S3Store) object(key string) string {
	if s.prefix == "" {
		return key
	}
	return s.prefix + "/" + key
}

// Put implements BlobStore.
func (s *S3Store) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, s.object(key), r, size, minio.PutObjectOptions{ContentType: contentType})
	return err
}

// Open implements BlobStore.
func (s *S3Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.client.GetObject(ctx, s.bucket, s.object(key), minio.GetObjectOptions{})
}

// Delete implements BlobStore.
func (s *S3Store) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, s.object(key), minio.RemoveObjectOptions{})
}

// BlobStoreFromEnv answers where attachments are kept: an S3-compatible bucket when
// GATOR_ATTACHMENTS_S3_ENDPOINT and _BUCKET are set (_ACCESS_KEY, _SECRET_KEY, _PREFIX,
// _INSECURE=1 for plain http), else a directory when GATOR_ATTACHMENTS_DIR is set, else nothing:
// a server without either takes no files.
func BlobStoreFromEnv(getenv func(string) string) (BlobStore, error) {
	if endpoint, bucket := getenv("GATOR_ATTACHMENTS_S3_ENDPOINT"), getenv("GATOR_ATTACHMENTS_S3_BUCKET"); endpoint != "" && bucket != "" {
		return NewS3Store(S3Config{
			Endpoint: endpoint, Bucket: bucket, Prefix: getenv("GATOR_ATTACHMENTS_S3_PREFIX"),
			AccessKey: getenv("GATOR_ATTACHMENTS_S3_ACCESS_KEY"), SecretKey: getenv("GATOR_ATTACHMENTS_S3_SECRET_KEY"),
			UseSSL: getenv("GATOR_ATTACHMENTS_S3_INSECURE") != "1",
		})
	}
	if dir := getenv("GATOR_ATTACHMENTS_DIR"); dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
		return DiskStore{Root: abs}, nil
	}
	return nil, nil
}

// randomKey names a stored file; nothing about it comes from the uploader.
func randomKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
