package caddys3proxy

import (
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// HeadHandler answers HEAD the way GetHandler answers GET, but issues an S3
// HeadObject instead of GetObject: no object body is transferred and the
// response carries headers only (Content-Type, Content-Length, ETag,
// Last-Modified, Cache-Control, …), as RFC 9110 §9.3.2 requires. Without it,
// ServeHTTP's method switch drops HEAD into its default arm and returns 405 —
// which link-checkers and RSS readers read as "URL does not exist".
//
// Hide list, directory-index resolution, and the 304-on-cached-index
// short-circuit mirror GetHandler, so a HEAD yields the same status a GET would
// for the same URL and conditional headers.
func (p S3Proxy) HeadHandler(w http.ResponseWriter, r *http.Request, fullPath string) error {
	if fileHidden(fullPath, p.Hide) {
		return caddyhttp.Error(http.StatusNotFound, nil)
	}

	isDir := strings.HasSuffix(fullPath, "/")
	var obj *s3.HeadObjectOutput
	var err error

	if isDir && len(p.IndexNames) > 0 {
		for _, indexPage := range p.IndexNames {
			indexPath := path.Join(fullPath, indexPage)
			obj, err = p.headS3Object(p.Bucket, indexPath, r.Header)
			caddyErr := convertToCaddyError(err)
			if caddyErr.StatusCode == http.StatusNotModified {
				// The client's cached copy of the index is still current —
				// return 304 directly, exactly as GetHandler does. Falling
				// through would leave obj == nil and re-head the directory key,
				// turning a valid 304 into a 404.
				return caddyErr
			}
			if err == nil {
				// We found an index!
				isDir = false
				break
			}
			logIt := true
			if aerr, ok := err.(awserr.Error); ok {
				// HeadObject reports a missing key as "NotFound" where
				// GetObject reports "NoSuchKey"; both are the common
				// "no index here" case and are not worth a warning.
				if aerr.Code() != s3.ErrCodeNoSuchKey && aerr.Code() != "NotFound" {
					logIt = false
				}
			}
			if logIt {
				p.log.Warn("error when looking for index",
					zap.String("bucket", p.Bucket),
					zap.String("key", fullPath),
					zap.String("err", err.Error()),
				)
			}
		}
	}

	// Still a directory: no index resolved.
	if isDir {
		if p.EnableBrowse {
			// A browsable directory exists, so the resource is a 200 — but a
			// HEAD response must carry no body, so the listing is not rendered.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			return nil
		}
		return caddyhttp.Error(http.StatusForbidden, errors.New("can not view a directory"))
	}

	// Head the object (skip if we already did while resolving an index).
	if obj == nil {
		obj, err = p.headS3Object(p.Bucket, fullPath, r.Header)
	}
	if err != nil {
		caddyErr := convertToCaddyError(err)
		switch caddyErr.StatusCode {
		case http.StatusNotFound:
			p.log.Debug("not found",
				zap.String("bucket", p.Bucket),
				zap.String("key", fullPath),
				zap.String("err", caddyErr.Error()),
			)
		case http.StatusNotModified, http.StatusPreconditionFailed, http.StatusRequestedRangeNotSatisfiable:
			// Conditional/range outcomes are normal client-cache behaviour;
			// ServeHTTP passes these status codes straight through.
			p.log.Debug("conditional request outcome",
				zap.Int("status", caddyErr.StatusCode),
				zap.String("bucket", p.Bucket),
				zap.String("key", fullPath),
			)
		default:
			p.log.Error("failed to head object",
				zap.String("bucket", p.Bucket),
				zap.String("key", fullPath),
				zap.String("err", caddyErr.Error()),
			)
		}
		return caddyErr
	}

	writeHeadersFromHeadObject(w, obj)
	return nil
}

// headS3Object mirrors getS3Object but issues a HeadObject, forwarding the same
// conditional request headers so caching behaviour matches GET.
//
// Range is deliberately NOT forwarded. A ranged HeadObject answers with the
// range's length in ContentLength, but s3.HeadObjectOutput has no ContentRange
// field (the SDK does not model that header for HeadObject, unlike
// GetObjectOutput), so we could not tell the client which range those bytes
// describe. Passing the range through anyway is what produced a Content-Length
// of the range with no Content-Range beside it — a HEAD that reports a 1 KiB
// object for a 4.7 MB video. A HEAD carries no body, so answering with the full
// representation's metadata is both truthful and the more useful answer.
func (p S3Proxy) headS3Object(bucket string, path string, headers http.Header) (*s3.HeadObjectOutput, error) {
	oi := &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(path),
	}

	if ifMatch := headers.Get("If-Match"); ifMatch != "" {
		oi = oi.SetIfMatch(ifMatch)
	}
	if ifNoneMatch := headers.Get("If-None-Match"); ifNoneMatch != "" {
		oi = oi.SetIfNoneMatch(ifNoneMatch)
	}
	if ifModifiedSince := headers.Get("If-Modified-Since"); ifModifiedSince != "" {
		if t, err := time.Parse(http.TimeFormat, ifModifiedSince); err == nil {
			oi = oi.SetIfModifiedSince(t)
		}
	}
	if ifUnmodifiedSince := headers.Get("If-Unmodified-Since"); ifUnmodifiedSince != "" {
		if t, err := time.Parse(http.TimeFormat, ifUnmodifiedSince); err == nil {
			oi = oi.SetIfUnmodifiedSince(t)
		}
	}

	p.log.Debug("head from S3",
		zap.String("bucket", bucket),
		zap.String("key", path),
	)

	return p.client.HeadObject(oi)
}

// writeHeadersFromHeadObject copies object metadata onto the response headers
// without a body. It is the headers-only twin of writeResponseFromGetObject;
// Content-Length is set explicitly here because there is no io.Copy to derive
// it from.
func writeHeadersFromHeadObject(w http.ResponseWriter, obj *s3.HeadObjectOutput) {
	setStrHeader(w, "Cache-Control", obj.CacheControl)
	setStrHeader(w, "Content-Disposition", obj.ContentDisposition)
	setStrHeader(w, "Content-Encoding", obj.ContentEncoding)
	setStrHeader(w, "Content-Language", obj.ContentLanguage)
	setStrHeader(w, "Content-Type", obj.ContentType)
	setStrHeader(w, "ETag", obj.ETag)
	setStrHeader(w, "Expires", obj.Expires)
	setTimeHeader(w, "Last-Modified", obj.LastModified)

	// Every S3 object is range-requestable; see writeResponseFromGetObject.
	// A media client that probes with HEAD decides here whether seeking is
	// even possible.
	w.Header().Set("Accept-Ranges", "bytes")

	// headS3Object does not forward Range, so this is the full object's size.
	if obj.ContentLength != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(*obj.ContentLength, 10))
	}

	for key, value := range obj.Metadata {
		setStrHeader(w, key, value)
	}
}
