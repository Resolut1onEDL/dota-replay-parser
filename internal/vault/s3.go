package vault

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Store is the Tigris-backed Store (any S3-compatible endpoint works).
type S3Store struct {
	client *s3.Client
	bucket string
}

// NewS3Store builds the client from cfg. Checksums are calculated only when an
// operation requires them: the S3-compatible endpoints do not all accept the
// newer default trailing checksums.
func NewS3Store(cfg Config) (*S3Store, error) {
	if !cfg.Enabled() {
		return nil, errors.New("vault: not configured")
	}
	opts := s3.Options{
		Region:                     cfg.Region,
		Credentials:                credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		UsePathStyle:               cfg.PathStyle,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if cfg.Endpoint != "" {
		opts.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	return &S3Store{client: s3.New(opts), bucket: cfg.Bucket}, nil
}

func isCode(err error, codes ...string) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		for _, c := range codes {
			if ae.ErrorCode() == c {
				return true
			}
		}
	}
	return false
}

func httpStatus(err error) int {
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

func notFound(err error) bool {
	var nf *types.NotFound
	var nk *types.NoSuchKey
	return errors.As(err, &nf) || errors.As(err, &nk) ||
		isCode(err, "NotFound", "NoSuchKey") || httpStatus(err) == http.StatusNotFound
}

func (s *S3Store) Head(ctx context.Context, key string) (ObjectInfo, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		if notFound(err) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, err
	}
	info := ObjectInfo{Key: key, Size: aws.ToInt64(out.ContentLength), Meta: copyMap(out.Metadata)}
	if out.LastModified != nil {
		info.LastModified = *out.LastModified
	}
	return info, nil
}

func (s *S3Store) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		if notFound(err) {
			return nil, ObjectInfo{}, ErrNotFound
		}
		return nil, ObjectInfo{}, err
	}
	info := ObjectInfo{Key: key, Size: aws.ToInt64(out.ContentLength), Meta: copyMap(out.Metadata)}
	if out.LastModified != nil {
		info.LastModified = *out.LastModified
	}
	return out.Body, info, nil
}

func (s *S3Store) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, opts PutOpts) error {
	in := &s3.PutObjectInput{
		Bucket:        &s.bucket,
		Key:           &key,
		Body:          body,
		ContentLength: aws.Int64(size),
		Metadata:      opts.Meta,
	}
	if opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	if opts.IfNoneMatch {
		in.IfNoneMatch = aws.String("*")
	}
	_, err := s.client.PutObject(ctx, in)
	if err != nil && opts.IfNoneMatch &&
		(isCode(err, "PreconditionFailed", "ConditionalRequestConflict") ||
			httpStatus(err) == http.StatusPreconditionFailed) {
		return ErrExists
	}
	return err
}

func (s *S3Store) copyOnto(ctx context.Context, dst, src string, meta map[string]string) error {
	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:            &s.bucket,
		Key:               &dst,
		CopySource:        aws.String(s.bucket + "/" + src),
		MetadataDirective: types.MetadataDirectiveReplace,
		Metadata:          meta,
	})
	return err
}

// CopyReplace rewrites metadata in place; if the endpoint refuses a copy onto
// the same key it goes through a temporary key and back.
func (s *S3Store) CopyReplace(ctx context.Context, key string, meta map[string]string) error {
	err := s.copyOnto(ctx, key, key, meta)
	if err == nil {
		return nil
	}
	if notFound(err) {
		return ErrNotFound
	}
	tmp := "_vault/tmp/" + strings.ReplaceAll(key, "/", "_")
	if err2 := s.copyOnto(ctx, tmp, key, meta); err2 != nil {
		return fmt.Errorf("copy-replace %s: %v; via temp key: %w", key, err, err2)
	}
	if err2 := s.copyOnto(ctx, key, tmp, meta); err2 != nil {
		return fmt.Errorf("copy-replace %s back from temp: %w", key, err2)
	}
	_ = s.Delete(ctx, tmp)
	return nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key})
	return err
}

func (s *S3Store) List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error {
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, o := range page.Contents {
			info := ObjectInfo{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size)}
			if o.LastModified != nil {
				info.LastModified = *o.LastModified
			}
			if err := fn(info); err != nil {
				return err
			}
		}
	}
	return nil
}
