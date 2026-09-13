// Package backup dumps PostgreSQL with pg_dump and uploads the archive to an S3-compatible
// bucket. Configured entirely from the environment; unconfigured means disabled.
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config from GATOR_BACKUP_* variables.
type Config struct {
	Endpoint  string // s3.eu-central-1.amazonaws.com or fsn1.your-objectstorage.com
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
	UseSSL    bool
	Keep      int // number of most recent dumps to keep; 0 = keep all
}

// LoadConfig reads the environment. Enabled() is false when bucket or endpoint is empty.
func LoadConfig() Config {
	keep := 0
	fmt.Sscanf(os.Getenv("GATOR_BACKUP_KEEP"), "%d", &keep)
	return Config{
		Endpoint:  os.Getenv("GATOR_BACKUP_S3_ENDPOINT"),
		Bucket:    os.Getenv("GATOR_BACKUP_S3_BUCKET"),
		Prefix:    strings.Trim(os.Getenv("GATOR_BACKUP_S3_PREFIX"), "/"),
		AccessKey: os.Getenv("GATOR_BACKUP_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("GATOR_BACKUP_S3_SECRET_KEY"),
		UseSSL:    os.Getenv("GATOR_BACKUP_S3_INSECURE") != "1",
		Keep:      keep,
	}
}

// Enabled reports whether backups are configured.
func (c Config) Enabled() bool { return c.Endpoint != "" && c.Bucket != "" }

// Dump runs pg_dump in custom format into dir and returns the file path.
func Dump(ctx context.Context, databaseURL, dir string) (string, error) {
	if _, err := exec.LookPath("pg_dump"); err != nil {
		return "", errors.New("pg_dump not found on PATH")
	}
	path := filepath.Join(dir, fmt.Sprintf("gator-%s.dump", time.Now().UTC().Format("20060102T150405Z")))
	cmd := exec.CommandContext(ctx, "pg_dump", "--format=custom", "--no-owner", "--no-privileges", "--file="+path, databaseURL)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pg_dump: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return path, nil
}

// Uploader stores dumps in the bucket.
type Uploader struct {
	cfg    Config
	client *minio.Client
}

// NewUploader connects to the bucket.
func NewUploader(cfg Config) (*Uploader, error) {
	c, err := minio.New(cfg.Endpoint, &minio.Options{Creds: credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""), Secure: cfg.UseSSL})
	if err != nil {
		return nil, err
	}
	return &Uploader{cfg: cfg, client: c}, nil
}

// Upload puts a local file into the bucket under prefix/basename.
func (u *Uploader) Upload(ctx context.Context, path string) (string, error) {
	key := filepath.Base(path)
	if u.cfg.Prefix != "" {
		key = u.cfg.Prefix + "/" + key
	}
	_, err := u.client.FPutObject(ctx, u.cfg.Bucket, key, path, minio.PutObjectOptions{ContentType: "application/octet-stream"})
	return key, err
}

// Prune deletes all but the newest cfg.Keep dumps under the prefix.
func (u *Uploader) Prune(ctx context.Context) (int, error) {
	if u.cfg.Keep <= 0 {
		return 0, nil
	}
	var keys []string
	for obj := range u.client.ListObjects(ctx, u.cfg.Bucket, minio.ListObjectsOptions{Prefix: u.cfg.Prefix, Recursive: true}) {
		if obj.Err != nil {
			return 0, obj.Err
		}
		if strings.HasSuffix(obj.Key, ".dump") {
			keys = append(keys, obj.Key)
		}
	}
	// names embed a UTC timestamp, so lexical order is chronological
	sortStrings(keys)
	deleted := 0
	for len(keys) > u.cfg.Keep {
		if err := u.client.RemoveObject(ctx, u.cfg.Bucket, keys[0], minio.RemoveObjectOptions{}); err != nil {
			return deleted, err
		}
		keys = keys[1:]
		deleted++
	}
	return deleted, nil
}

// Run performs one full backup: dump, upload, prune, remove local file.
func Run(ctx context.Context, cfg Config, databaseURL string, log *slog.Logger) error {
	dir, err := os.MkdirTemp("", "gator-backup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path, err := Dump(ctx, databaseURL, dir)
	if err != nil {
		return err
	}
	st, _ := os.Stat(path)
	up, err := NewUploader(cfg)
	if err != nil {
		return err
	}
	key, err := up.Upload(ctx, path)
	if err != nil {
		return err
	}
	pruned, err := up.Prune(ctx)
	if err != nil {
		log.Warn("backup prune failed", "err", err)
	}
	log.Info("backup uploaded", "key", key, "bytes", st.Size(), "pruned", pruned)
	return nil
}

// Restore replays a dump into databaseURL. It converts the custom-format archive to SQL with
// pg_restore, drops session settings a newer client emits that an older server rejects
// (e.g. transaction_timeout), and applies it with psql under ON_ERROR_STOP.
func Restore(ctx context.Context, databaseURL, path string) error {
	for _, bin := range []string{"pg_restore", "psql"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s not found on PATH", bin)
		}
	}
	dump := exec.CommandContext(ctx, "pg_restore", "--clean", "--if-exists", "--no-owner", "--no-privileges", "-f", "-", path)
	sql, err := dump.Output()
	if err != nil {
		return fmt.Errorf("pg_restore: %w", err)
	}
	var filtered strings.Builder
	for _, line := range strings.Split(string(sql), "\n") {
		if isUnportableSetting(line) {
			continue
		}
		filtered.WriteString(line)
		filtered.WriteByte('\n')
	}
	apply := exec.CommandContext(ctx, "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", databaseURL)
	apply.Stdin = strings.NewReader(filtered.String())
	var stderr strings.Builder
	apply.Stderr = &stderr
	if err := apply.Run(); err != nil {
		return fmt.Errorf("psql restore: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// isUnportableSetting matches SET lines for parameters that only newer servers know.
func isUnportableSetting(line string) bool {
	for _, p := range []string{"transaction_timeout", "idle_in_transaction_session_timeout"} {
		if strings.HasPrefix(line, "SET "+p+" ") {
			return true
		}
	}
	return false
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

var _ io.Reader = (*os.File)(nil)
