package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Config configures an S3-compatible bucket.
//
// Cloudflare R2: Endpoint https://<account id>.r2.cloudflarestorage.com,
// Region "auto". AWS S3: leave Endpoint empty and set Region. MinIO: set
// Endpoint and PathStyle.
type S3Config struct {
	Bucket          string
	Region          string
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	Prefix          string // key prefix, e.g. "uploads/"
	PathStyle       bool
}

// S3 stores files as objects in a bucket.
type S3 struct {
	c      *s3.Client
	bucket string
	prefix string
}

func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("S3_BUCKET is required when STORAGE=s3")
	}
	if cfg.Region == "" {
		cfg.Region = "auto"
	}
	opts := s3.Options{
		Region:       cfg.Region,
		UsePathStyle: cfg.PathStyle,
		// Only send/verify checksums when an API requires them: older
		// S3-compatible servers reject the newer default CRC headers.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		RetryMaxAttempts:           3,
	}
	if cfg.AccessKeyID != "" {
		opts.Credentials = credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")
	} else {
		return nil, errors.New("S3_ACCESS_KEY_ID and S3_SECRET_ACCESS_KEY are required when STORAGE=s3")
	}
	if cfg.Endpoint != "" {
		opts.BaseEndpoint = aws.String(strings.TrimRight(cfg.Endpoint, "/"))
	}
	return &S3{c: s3.New(opts), bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

func (s *S3) key(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return "", ErrNotExist
	}
	return s.prefix + name, nil
}

// Put uploads the object. Non-seekable readers are buffered (uploads are at
// most 25 MB) so the request can be signed and retried.
func (s *S3) Put(ctx context.Context, name string, r io.Reader, contentType string) error {
	key, err := s.key(name)
	if err != nil {
		return err
	}
	body, ok := r.(io.ReadSeeker)
	if !ok {
		b, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	in := &s3.PutObjectInput{
		Bucket:       aws.String(s.bucket),
		Key:          aws.String(key),
		Body:         body,
		CacheControl: aws.String("public, max-age=31536000, immutable"),
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	if _, err := s.c.PutObject(ctx, in); err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	return nil
}

func (s *S3) Open(ctx context.Context, name string) (File, error) {
	key, err := s.key(name)
	if err != nil {
		return nil, err
	}
	out, err := s.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("s3 %s: %w", key, ErrNotExist)
		}
		return nil, fmt.Errorf("s3 head %s: %w", key, err)
	}
	f := &s3File{ctx: ctx, s: s, key: key, size: aws.ToInt64(out.ContentLength)}
	if out.LastModified != nil {
		f.mod = *out.LastModified
	}
	return f, nil
}

func (s *S3) List(ctx context.Context) ([]string, error) {
	var out []string
	p := s3.NewListObjectsV2Paginator(s.c, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(s.prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3 list: %w", err)
		}
		for _, o := range page.Contents {
			if n := strings.TrimPrefix(aws.ToString(o.Key), s.prefix); n != "" && !strings.Contains(n, "/") {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

func isNotFound(err error) bool {
	var re *awshttp.ResponseError
	return errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound
}

// s3File reads lazily: each Read after a Seek opens a ranged GET from the
// current offset, so http.ServeContent's size probe costs nothing and a Range
// request downloads only the bytes it sends.
type s3File struct {
	ctx  context.Context
	s    *S3
	key  string
	size int64
	mod  time.Time
	off  int64
	body io.ReadCloser
}

func (f *s3File) Size() int64        { return f.size }
func (f *s3File) ModTime() time.Time { return f.mod }

func (f *s3File) Read(p []byte) (int, error) {
	if f.off >= f.size {
		return 0, io.EOF
	}
	if f.body == nil {
		out, err := f.s.c.GetObject(f.ctx, &s3.GetObjectInput{
			Bucket: aws.String(f.s.bucket),
			Key:    aws.String(f.key),
			Range:  aws.String(fmt.Sprintf("bytes=%d-", f.off)),
		})
		if err != nil {
			return 0, fmt.Errorf("s3 get %s: %w", f.key, err)
		}
		f.body = out.Body
	}
	n, err := f.body.Read(p)
	f.off += int64(n)
	if err == io.EOF && f.off < f.size {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (f *s3File) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.off + offset
	case io.SeekEnd:
		abs = f.size + offset
	default:
		return 0, errors.New("s3File.Seek: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("s3File.Seek: negative position")
	}
	if abs != f.off && f.body != nil {
		f.body.Close()
		f.body = nil
	}
	f.off = abs
	return abs, nil
}

func (f *s3File) Close() error {
	if f.body != nil {
		return f.body.Close()
	}
	return nil
}
