package openapi3filter

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// GzipRequestBodyOptions configures explicit validation of JSON request
// bodies ("application/json" or a media type with the "+json" suffix,
// RFC 6839) that are sent with a "Content-Encoding: gzip" header.
//
// The compressed request body is read and decompressed for schema
// validation only; on success the original compressed bytes are handed
// back to the caller unchanged, with Content-Encoding preserved. A body
// whose size is exactly equal to a configured limit is accepted.
//
// Limits are enforced from actual bytes read. Neither Content-Length nor
// the gzip header/trailer size declarations are trusted.
type GzipRequestBodyOptions struct {
	// MaxCompressedBytes bounds the number of compressed bytes read from
	// the request body. A value <= 0 means no limit. Reading stops as soon
	// as more than this many bytes have been observed; the remaining body
	// is not drained.
	MaxCompressedBytes int64

	// MaxDecompressedBytes bounds the number of bytes produced while
	// decompressing the request body. A value <= 0 means no limit.
	// Decompressing stops as soon as more than this many bytes have been
	// produced.
	MaxDecompressedBytes int64
}

// Sentinel errors for the gzip request-body validation mode. All errors
// returned by that mode wrap one of these, and some wrap additional typed
// errors carrying details; use errors.Is / errors.As to distinguish:
//
//   - ErrUnsupportedContentEncoding: unsupported or stacked content coding
//     (rejected before the body is read)
//   - ErrGzipBodyCorrupt: malformed/truncated gzip data, bad checksum, a
//     concatenated second gzip member or trailing garbage
//   - ErrRequestBodyTooLarge: a compressed-input or decompressed-output
//     byte limit was exceeded
//   - ErrRequestBodyRead: the underlying request body reader failed
//
// Schema failures keep using the regular openapi3 schema error.
var (
	ErrUnsupportedContentEncoding = errors.New("unsupported request content encoding")
	ErrGzipBodyCorrupt            = errors.New("corrupt gzip request body")
	ErrRequestBodyTooLarge        = errors.New("request body exceeds the configured size limit")
	ErrRequestBodyRead            = errors.New("reading request body failed")
)

var headerContentEncoding = http.CanonicalHeaderKey("Content-Encoding")

// ContentEncodingError reports a Content-Encoding value that the gzip
// request-body validation mode does not accept. The body is not read when
// this error is returned.
type ContentEncodingError struct {
	// Encoding is the offending raw Content-Encoding header value
	// (values from multiple header fields are joined with ", ").
	Encoding string
	Reason   string
}

func (e *ContentEncodingError) Error() string {
	if e.Encoding == "" {
		return e.Reason
	}
	return fmt.Sprintf("%s (Content-Encoding %q)", e.Reason, e.Encoding)
}

// Unwrap marks the error as ErrUnsupportedContentEncoding.
func (e *ContentEncodingError) Unwrap() error { return ErrUnsupportedContentEncoding }

// GzipBodyError reports corrupt gzip data: an invalid header, truncated
// member, checksum mismatch, a concatenated second member, or trailing
// garbage. Err, when non-nil, is the underlying error from the gzip
// reader (for example gzip.ErrChecksum or io.ErrUnexpectedEOF).
type GzipBodyError struct {
	Reason string
	Err    error
}

func (e *GzipBodyError) Error() string {
	if e.Err != nil {
		return e.Reason + ": " + e.Err.Error()
	}
	return e.Reason
}

// Unwrap marks the error as ErrGzipBodyCorrupt while still exposing the
// underlying gzip error.
func (e *GzipBodyError) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrGzipBodyCorrupt}
	}
	return []error{ErrGzipBodyCorrupt, e.Err}
}

// BodyLimitError reports that a request-body byte limit was exceeded.
type BodyLimitError struct {
	// Compressed is true when MaxCompressedBytes was exceeded while
	// reading the request, and false when MaxDecompressedBytes was
	// exceeded during decompression.
	Compressed bool
	// Limit is the configured limit.
	Limit int64
	// Read is the number of bytes observed when the limit was exceeded
	// (always greater than Limit).
	Read int64
	// BodyComplete reports whether the entire original compressed request
	// body was nonetheless read and retained, so the request can be
	// replayed from GetBody. It is false when reading the compressed
	// stream was interrupted (compressed-input limit). A
	// decompressed-output overflow leaves the fully-read compressed body
	// replayable, so BodyComplete is true in that case.
	BodyComplete bool
}

func (e *BodyLimitError) Error() string {
	stage := "compressed input"
	if !e.Compressed {
		stage = "decompressed output"
	}
	return fmt.Sprintf("%s size %d bytes exceeds the limit of %d bytes", stage, e.Read, e.Limit)
}

// Unwrap marks the error as ErrRequestBodyTooLarge.
func (e *BodyLimitError) Unwrap() error { return ErrRequestBodyTooLarge }

// BodyReadError wraps an error returned by the underlying request body
// reader. The compressed body was not fully read, so it is not replayable.
type BodyReadError struct {
	Err error
}

func (e *BodyReadError) Error() string {
	return ErrRequestBodyRead.Error() + ": " + e.Err.Error()
}

// Unwrap marks the error as ErrRequestBodyRead while still exposing the
// underlying transport error.
func (e *BodyReadError) Unwrap() []error {
	return []error{ErrRequestBodyRead, e.Err}
}

// gzipBodyReadChunkSize bounds how far a single Read call may overshoot a
// configured byte limit.
const gzipBodyReadChunkSize = 32 * 1024

// parseRequestContentEncoding inspects only the Content-Encoding header
// without touching the request body. It returns "" for an absent, empty or
// "identity" coding, "gzip" for a single gzip token, or an error for any
// other coding, a coding stack, or multiple Content-Encoding fields.
func parseRequestContentEncoding(header http.Header) (string, error) {
	values := header.Values(headerContentEncoding)
	switch len(values) {
	case 0:
		return "", nil
	case 1:
	default:
		return "", &ContentEncodingError{
			Encoding: strings.Join(values, ", "),
			Reason:   "multiple Content-Encoding header fields are not supported; only identity or a single gzip coding is accepted",
		}
	}

	raw := values[0]
	var tokens []string
	for _, part := range strings.Split(raw, ",") {
		token := strings.ToLower(strings.TrimSpace(part))
		if token != "" {
			tokens = append(tokens, token)
		}
	}
	switch len(tokens) {
	case 0:
		// A whitespace-only value carries no content coding.
		return "", nil
	case 1:
	default:
		return "", &ContentEncodingError{
			Encoding: raw,
			Reason:   "content-coding stacks are not supported; only identity or a single gzip coding is accepted",
		}
	}

	switch tokens[0] {
	case "identity":
		return "", nil
	case "gzip":
		return "gzip", nil
	default:
		return "", &ContentEncodingError{
			Encoding: raw,
			Reason:   fmt.Sprintf("unsupported content coding %q; only identity or gzip is accepted", tokens[0]),
		}
	}
}

// isJSONMediaType reports whether mediaType (a Content-Type value, with or
// without parameters) denotes JSON.
func isJSONMediaType(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

// readCompressedBody reads r fully into dst in bounded chunks. Bytes are
// counted from actual Read calls: reading stops as soon as more than limit
// bytes (limit <= 0 means no limit) have been observed, without draining
// the rest of r. A body of exactly limit bytes is accepted.
func readCompressedBody(r io.Reader, limit int64, dst *bytes.Buffer) (int64, error) {
	var total int64
	buf := make([]byte, gzipBodyReadChunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			total += int64(n)
			dst.Write(buf[:n])
			if limit > 0 && total > limit {
				return total, &BodyLimitError{
					Compressed:   true,
					Limit:        limit,
					Read:         total,
					BodyComplete: false,
				}
			}
		}
		switch {
		case err == io.EOF:
			return total, nil
		case err != nil:
			return total, &BodyReadError{Err: err}
		}
	}
}

// countedByteReader counts every byte consumed through Read and ReadByte.
// Implementing io.ByteReader makes gzip/flate consume the source one byte
// at a time, so no bytes past the end of the first gzip member are pulled
// in ahead; this allows exact detection of a second member or trailing
// garbage.
type countedByteReader struct {
	r     io.Reader
	br    io.ByteReader
	count int64
}

func (c *countedByteReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.count += int64(n)
	return n, err
}

func (c *countedByteReader) ReadByte() (byte, error) {
	b, err := c.br.ReadByte()
	if err == nil {
		c.count++
	}
	return b, err
}

// gunzipSingleMember decompresses exactly one gzip member. It enforces the
// decompressed-output byte limit (0 = unlimited; exact equality allowed)
// from bytes actually produced, verifies the gzip trailer checksum and
// size, and rejects a concatenated second member as well as any trailing
// garbage. memberBytes is the number of compressed bytes consumed by the
// first member.
func gunzipSingleMember(compressed []byte, limit int64) (out []byte, memberBytes int64, err error) {
	raw := bytes.NewReader(compressed)
	src := &countedByteReader{r: raw, br: raw}

	zr, err := gzip.NewReader(src)
	if err != nil {
		return nil, src.count, &GzipBodyError{Reason: "invalid gzip header", Err: err}
	}
	// Do not transparently continue into a concatenated second member.
	zr.Multistream(false)

	var decompressed bytes.Buffer
	buf := make([]byte, gzipBodyReadChunkSize)
	var total int64
	for {
		n, readErr := zr.Read(buf)
		if n > 0 {
			total += int64(n)
			decompressed.Write(buf[:n])
			if limit > 0 && total > limit {
				return decompressed.Bytes(), src.count, &BodyLimitError{
					Compressed:   false,
					Limit:        limit,
					Read:         total,
					BodyComplete: true,
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			// Truncated input surfaces as io.ErrUnexpectedEOF, damaged
			// data as gzip.ErrChecksum (the trailer CRC32/ISIZE are
			// checked when the member ends); both mean corrupt data.
			return decompressed.Bytes(), src.count, &GzipBodyError{Reason: "gzip stream is corrupt or truncated", Err: readErr}
		}
	}

	if remaining := int64(len(compressed)) - src.count; remaining != 0 {
		return decompressed.Bytes(), src.count, &GzipBodyError{
			Reason: fmt.Sprintf("gzip stream has %d trailing byte(s) after the first member: concatenated members and trailing garbage are not accepted", remaining),
		}
	}
	return decompressed.Bytes(), src.count, nil
}

// replayBody installs data as the in-memory request body: ContentLength
// reflects the actual byte count (Content-Length is not trusted) and every
// GetBody call returns an independent reader over the same bytes.
func replayBody(req *http.Request, data []byte) {
	req.ContentLength = int64(len(data))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	req.Body, _ = req.GetBody()
}

// markBodyIncomplete is used when the original compressed body could not be
// fully obtained (over-limit or a read failure): it exposes only the bytes
// actually received and clears GetBody so callers cannot be led to believe
// the unknown remainder is replayable.
func markBodyIncomplete(req *http.Request, partial []byte) {
	req.ContentLength = -1
	req.GetBody = nil
	req.Body = io.NopCloser(bytes.NewReader(partial))
}

// validateGzipRequestBody implements the gzip request-body validation mode.
// It returns handled=false for an absent/identity content coding so the
// caller runs the regular validation path unchanged.
func validateGzipRequestBody(input *RequestValidationInput, requestBody *openapi3.RequestBody, options *Options) (handled bool, err error) {
	req := input.Request

	coding, err := parseRequestContentEncoding(req.Header)
	if err != nil {
		// Reject before reading the body.
		req.Body.Close()
		return true, &RequestError{
			Input:       input,
			RequestBody: requestBody,
			Reason:      "unsupported content encoding",
			Err:         err,
		}
	}
	if coding == "" {
		// identity or no Content-Encoding: default behavior.
		return false, nil
	}

	originalBody := req.Body
	defer originalBody.Close()

	inputMIME := req.Header.Get(headerCT)
	if !isJSONMediaType(inputMIME) {
		return true, &RequestError{
			Input:       input,
			RequestBody: requestBody,
			Reason:      "unsupported content encoding",
			Err: &ContentEncodingError{
				Encoding: "gzip",
				Reason:   "gzip content coding is only accepted for JSON request bodies (application/json or +json)",
			},
		}
	}

	// Prefer a fresh GetBody copy when one is available (as created by
	// http.NewRequest or by a previous validation run), so validation also
	// works when req.Body was already consumed.
	readFrom := originalBody
	if req.GetBody != nil {
		if fresh, freshErr := req.GetBody(); freshErr == nil {
			readFrom = fresh
			defer fresh.Close()
		}
	}

	// 1. Read the compressed bytes with a hard input limit, counted from
	//    actual reads. Content-Length and gzip headers are never trusted.
	var compressed bytes.Buffer
	if _, readErr := readCompressedBody(readFrom, options.GzipRequestBody.MaxCompressedBytes, &compressed); readErr != nil {
		raw := compressed.Bytes()
		if _, ok := readErr.(*BodyReadError); ok || isCompressedLimit(readErr) {
			markBodyIncomplete(req, raw)
		} else {
			replayBody(req, raw)
		}
		return true, &RequestError{
			Input:       input,
			RequestBody: requestBody,
			Reason:      requestBodyErrorReason(readErr),
			Err:         readErr,
		}
	}
	raw := compressed.Bytes()

	// From here on the full original compressed body is in memory and can
	// always be replayed, whatever happens next.
	restore := func() { replayBody(req, raw) }

	// 2. Decompress a single member, enforcing the output limit and
	//    validating the gzip trailer and stream framing.
	data, _, gzipErr := gunzipSingleMember(raw, options.GzipRequestBody.MaxDecompressedBytes)
	if gzipErr != nil {
		restore()
		return true, &RequestError{
			Input:       input,
			RequestBody: requestBody,
			Reason:      requestBodyErrorReason(gzipErr),
			Err:         gzipErr,
		}
	}

	if len(data) == 0 {
		restore()
		if requestBody.Required {
			return true, &RequestError{Input: input, RequestBody: requestBody, Err: ErrInvalidRequired}
		}
		return true, nil
	}

	content := requestBody.Content
	if len(content) == 0 {
		// A request's body does not have declared content, so skip validation.
		restore()
		return true, nil
	}

	contentType := content.Get(inputMIME)
	if contentType == nil {
		restore()
		return true, &RequestError{
			Input:       input,
			RequestBody: requestBody,
			Reason:      fmt.Sprintf("%s %q", prefixInvalidCT, inputMIME),
		}
	}

	if contentType.Schema == nil {
		// No JSON schema declared, so skip validation.
		restore()
		return true, nil
	}

	// 3. Decode and validate the decompressed bytes against the schema. The
	//    Content-Encoding header is never mistaken for Content-Type: the
	//    decoder only sees Content-Type and the decompressed bytes.
	encFn := func(name string) *openapi3.Encoding { return contentType.Encoding[name] }
	mediaType, value, err := decodeBody(bytes.NewReader(data), req.Header, contentType.Schema, encFn)
	if err != nil {
		restore()
		return true, &RequestError{
			Input:       input,
			RequestBody: requestBody,
			Reason:      "failed to decode request body",
			Err:         err,
		}
	}

	defaultsSet := false
	var opts []openapi3.SchemaValidationOption
	opts = append(opts, openapi3.VisitAsRequest())
	// Defaults are written back into the body, which needs an encoder.
	if !options.SkipSettingDefaults && (RegisteredBodyEncoder(mediaType) != nil || strings.HasSuffix(mediaType, "+json")) {
		opts = append(opts, openapi3.DefaultsSet(func() { defaultsSet = true }))
	}
	if options.MultiError {
		opts = append(opts, openapi3.MultiErrors())
	}
	if options.customSchemaErrorFunc != nil {
		opts = append(opts, openapi3.SetSchemaErrorMessageCustomizer(options.customSchemaErrorFunc))
	}
	if options.ExcludeReadOnlyValidations {
		opts = append(opts, openapi3.DisableReadOnlyValidation())
	}
	if options.RegexCompiler != nil {
		opts = append(opts, openapi3.SetSchemaRegexCompiler(options.RegexCompiler))
	}
	opts = append(opts, options.SchemaValidationOptions...)
	if input.Route != nil && input.Route.Spec.IsOpenAPI31OrLater() {
		opts = append(opts, openapi3.EnableJSONSchema2020())
	}

	if err := contentType.Schema.Value.VisitJSON(value, opts...); err != nil {
		restore()
		schemaId := getSchemaIdentifier(contentType.Schema)
		schemaId = prependSpaceIfNeeded(schemaId)
		return true, &RequestError{
			Input:       input,
			RequestBody: requestBody,
			Reason:      fmt.Sprintf("doesn't match schema%s", schemaId),
			Err:         err,
		}
	}

	// 4. Hand the body back to downstream handlers. Without injected
	//    defaults the original compressed bytes are returned byte for
	//    byte and Content-Encoding stays "gzip". When defaults actually
	//    changed the JSON value, the modified JSON is re-encoded and
	//    compressed again: JSON is never written into the compressed
	//    bytes and the stale length is never reused.
	if defaultsSet {
		plain, err := encodeBody(value, mediaType)
		if err != nil {
			restore()
			return true, &RequestError{
				Input:       input,
				RequestBody: requestBody,
				Reason:      "rewriting failed",
				Err:         err,
			}
		}
		var encoded bytes.Buffer
		zw := gzip.NewWriter(&encoded)
		if _, err := zw.Write(plain); err != nil {
			restore()
			return true, &RequestError{
				Input:       input,
				RequestBody: requestBody,
				Reason:      "rewriting failed",
				Err:         err,
			}
		}
		if err := zw.Close(); err != nil {
			restore()
			return true, &RequestError{
				Input:       input,
				RequestBody: requestBody,
				Reason:      "rewriting failed",
				Err:         err,
			}
		}
		replayBody(req, encoded.Bytes())
	} else {
		restore()
	}

	if options.KeepValidatedValue {
		// The validated value comes from the decompressed body, but the
		// body bytes exposed downstream remain gzip.
		req.Body = validatedBody{ReadCloser: req.Body, value: value}
	}
	return true, nil
}

func isCompressedLimit(err error) bool {
	limitErr, ok := err.(*BodyLimitError)
	return ok && limitErr.Compressed
}

func requestBodyErrorReason(err error) string {
	switch {
	case errors.Is(err, ErrRequestBodyRead):
		return "reading failed"
	case errors.Is(err, ErrRequestBodyTooLarge):
		return "request body too large"
	case errors.Is(err, ErrGzipBodyCorrupt):
		return "failed to decompress request body"
	default:
		return "request body error"
	}
}
