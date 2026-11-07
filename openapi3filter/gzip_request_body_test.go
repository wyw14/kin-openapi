package openapi3filter

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/getkin/kin-openapi/openapi3"
)

const gzipTestSpec = `
openapi: 3.0.0
info:
  title: gzip test
  version: '1'
paths:
  /things:
    post:
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [id]
              properties:
                id: {type: integer}
                name: {type: string, default: default-name}
                data: {type: string}
      responses:
        '200': {description: ok}
  /things-plus:
    post:
      requestBody:
        required: true
        content:
          application/vnd.kin+json:
            schema:
              type: object
              required: [id]
              properties:
                id: {type: integer}
                name: {type: string, default: default-name}
      responses:
        '200': {description: ok}
`

func gzipTestOptions() *Options {
	return &Options{
		GzipRequestBody: &GzipRequestBodyOptions{
			MaxCompressedBytes:   16 << 20,
			MaxDecompressedBytes: 16 << 20,
		},
	}
}

// gzipPassThroughOptions disables default injection so a valid body is
// handed back byte-for-byte.
func gzipPassThroughOptions() *Options {
	o := gzipTestOptions()
	o.SkipSettingDefaults = true
	return o
}

func mustGzip(t *testing.T, plain []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	_, err := zw.Write(plain)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return b.Bytes()
}

func mustGunzip(t *testing.T, compressed []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	require.NoError(t, err)
	plain, err := io.ReadAll(zr)
	require.NoError(t, err)
	require.NoError(t, zr.Close())
	return plain
}

// perByteReader returns at most one byte per Read call, so gzip data
// arrives split across many chunks.
type perByteReader struct{ r io.Reader }

func (p *perByteReader) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	return p.r.Read(b[:1])
}

// countingReadCloser counts bytes and Read calls pulled from the
// underlying request body.
type countingReadCloser struct {
	r     io.Reader
	bytes int64
	calls int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	c.calls++
	n, err := c.r.Read(p)
	c.bytes += int64(n)
	return n, err
}
func (c *countingReadCloser) Close() error { return nil }

// failAfterReader hands out its data one byte at a time and then returns
// fail on every Read (the last byte is delivered together with the error).
type failAfterReader struct {
	data []byte
	pos  int
	fail error
}

func (r *failAfterReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, r.fail
	}
	p[0] = r.data[r.pos]
	r.pos++
	if r.pos == len(r.data) {
		return 1, r.fail
	}
	return 1, nil
}
func (r *failAfterReader) Close() error { return nil }

func newGzipRequest(t *testing.T, path string, body io.Reader, contentType string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, path, body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Content-Encoding", "gzip")
	return req
}

func validateGzipRequest(t *testing.T, req *http.Request, options *Options) error {
	t.Helper()
	router := setupTestRouter(t, gzipTestSpec)
	route, pathParams, err := router.FindRoute(req)
	require.NoError(t, err)
	return ValidateRequest(context.Background(), &RequestValidationInput{
		Request:    req,
		PathParams: pathParams,
		Route:      route,
		Options:    options,
	})
}

func TestGzipRequestBodyPassThrough(t *testing.T) {
	t.Parallel()

	plain := []byte(`{"id":1}`)
	gz := mustGzip(t, plain)

	req := newGzipRequest(t, "/things", bytes.NewReader(gz), "application/json")
	// Claim a wrong Content-Length: the validator must not trust it.
	req.ContentLength = 42

	require.NoError(t, validateGzipRequest(t, req, gzipPassThroughOptions()))

	// Downstream still sees a gzip request.
	require.Equal(t, "gzip", req.Header.Get("Content-Encoding"))
	require.Equal(t, "application/json", req.Header.Get("Content-Type"))
	require.Equal(t, int64(len(gz)), req.ContentLength)

	// Current body replays the original compressed bytes.
	got, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, gz, got)
	require.Equal(t, plain, mustGunzip(t, got))

	// Every GetBody call returns an independent reader: two readers can be
	// consumed interleaved without interfering.
	require.NotNil(t, req.GetBody)
	b1, err := req.GetBody()
	require.NoError(t, err)
	b2, err := req.GetBody()
	require.NoError(t, err)

	head := make([]byte, 3)
	n, err := io.ReadFull(b1, head)
	require.NoError(t, err)
	require.Equal(t, 3, n)

	all2, err := io.ReadAll(b2)
	require.NoError(t, err)
	require.Equal(t, gz, all2)

	rest1, err := io.ReadAll(b1)
	require.NoError(t, err)
	require.Equal(t, gz, bytes.Join([][]byte{head, rest1}, nil))

	// A third fresh reader after the first two were consumed.
	b3, err := req.GetBody()
	require.NoError(t, err)
	all3, err := io.ReadAll(b3)
	require.NoError(t, err)
	require.Equal(t, gz, all3)
}

func TestGzipRequestBodyAcrossChunksAndReadCount(t *testing.T) {
	t.Parallel()

	// Compressible payload; delivered one byte per Read call.
	plain := []byte(`{"id":1,"data":"` + strings.Repeat("abc123", 8000) + `"}`)
	gz := mustGzip(t, plain)

	counter := &countingReadCloser{r: &perByteReader{r: bytes.NewReader(gz)}}
	req := newGzipRequest(t, "/things", counter, "application/json; charset=utf-8")
	// Deliberately bogus length.
	req.ContentLength = -1

	require.NoError(t, validateGzipRequest(t, req, gzipPassThroughOptions()))

	// Exactly the compressed bytes were pulled from the transport, one
	// (small) chunk at a time; the reader was not drained beyond EOF with
	// unbounded reads.
	require.Equal(t, int64(len(gz)), counter.bytes)
	require.Equal(t, int64(len(gz)+1), counter.calls) // one final EOF call

	got, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, gz, got)
	require.Equal(t, plain, mustGunzip(t, got))
	require.Equal(t, int64(len(gz)), req.ContentLength)
}

func TestGzipRequestBodyPlusJSON(t *testing.T) {
	t.Parallel()

	plain := []byte(`{"id":99,"name":"plus"}`)
	gz := mustGzip(t, plain)
	req := newGzipRequest(t, "/things-plus", bytes.NewReader(gz), "application/vnd.kin+json")

	require.NoError(t, validateGzipRequest(t, req, gzipTestOptions()))
	require.Equal(t, "gzip", req.Header.Get("Content-Encoding"))
	got, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, gz, got)
}

func TestGzipRequestBodyExactLimitsAreLegal(t *testing.T) {
	t.Parallel()

	plain := []byte(`{"id":1}`)
	gz := mustGzip(t, plain)

	options := gzipPassThroughOptions()
	options.GzipRequestBody.MaxCompressedBytes = int64(len(gz))
	options.GzipRequestBody.MaxDecompressedBytes = int64(len(plain))
	req := newGzipRequest(t, "/things", bytes.NewReader(gz), "application/json")
	require.NoError(t, validateGzipRequest(t, req, options))

	got, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, gz, got)

	// One byte over each limit is rejected (decompressed).
	options2 := gzipPassThroughOptions()
	options2.GzipRequestBody.MaxCompressedBytes = int64(len(gz))
	options2.GzipRequestBody.MaxDecompressedBytes = int64(len(plain)) - 1
	req2 := newGzipRequest(t, "/things", bytes.NewReader(gz), "application/json")
	err = validateGzipRequest(t, req2, options2)
	require.Error(t, err)
	var limitErr *BodyLimitError
	require.ErrorAs(t, err, &limitErr)
	require.False(t, limitErr.Compressed)
	require.Equal(t, int64(len(plain)), limitErr.Read)
	require.True(t, errors.Is(err, ErrRequestBodyTooLarge))

	// One byte over the compressed limit (1-byte chunks -> exact count).
	counter := &countingReadCloser{r: &perByteReader{r: bytes.NewReader(gz)}}
	options3 := gzipPassThroughOptions()
	options3.GzipRequestBody.MaxCompressedBytes = int64(len(gz)) - 1
	req3 := newGzipRequest(t, "/things", counter, "application/json")
	err = validateGzipRequest(t, req3, options3)
	require.Error(t, err)
	require.ErrorAs(t, err, &limitErr)
	require.True(t, limitErr.Compressed)
	require.Equal(t, int64(len(gz)), limitErr.Read)
	// Stopped immediately; the rest was not drained.
	require.Equal(t, int64(len(gz)), counter.bytes)
}

func TestGzipRequestBodyDecompressedLimitZipBomb(t *testing.T) {
	t.Parallel()

	// Tiny compressed body that expands to ~1 MiB.
	plain := []byte(`{"id":1,"data":"` + strings.Repeat("\x00", 1<<20) + `"}`)
	gz := mustGzip(t, plain)
	require.Less(t, len(gz), 8<<10)

	options := &Options{GzipRequestBody: &GzipRequestBodyOptions{
		MaxCompressedBytes:   16 << 20,
		MaxDecompressedBytes: 4096,
	}}
	req := newGzipRequest(t, "/things", bytes.NewReader(gz), "application/json")
	err := validateGzipRequest(t, req, options)
	require.Error(t, err)

	var limitErr *BodyLimitError
	require.ErrorAs(t, err, &limitErr)
	require.False(t, limitErr.Compressed)
	require.Equal(t, int64(4096), limitErr.Limit)
	require.Greater(t, limitErr.Read, int64(4096))
	// Bounded chunk loop: overshoot can never exceed the chunk size.
	require.LessOrEqual(t, limitErr.Read, int64(4096)+gzipBodyReadChunkSize)
	require.True(t, errors.Is(err, ErrRequestBodyTooLarge))

	// The complete compressed body was obtained and stays replayable.
	require.True(t, limitErr.BodyComplete)
	require.NotNil(t, req.GetBody)
	again, err := req.GetBody()
	require.NoError(t, err)
	replayed, err := io.ReadAll(again)
	require.NoError(t, err)
	require.Equal(t, gz, replayed)
	require.Equal(t, "gzip", req.Header.Get("Content-Encoding"))
}

func TestGzipRequestBodyCompressedLimitStopsEarly(t *testing.T) {
	t.Parallel()

	plain := []byte(`{"id":1,"data":"` + strings.Repeat("x", 5000) + `"}`)
	gz := mustGzip(t, plain)
	const limit = int64(50)

	counter := &countingReadCloser{r: &perByteReader{r: bytes.NewReader(gz)}}
	options := &Options{GzipRequestBody: &GzipRequestBodyOptions{
		MaxCompressedBytes:   limit,
		MaxDecompressedBytes: 16 << 20,
	}}
	req := newGzipRequest(t, "/things", counter, "application/json")
	err := validateGzipRequest(t, req, options)
	require.Error(t, err)

	var limitErr *BodyLimitError
	require.ErrorAs(t, err, &limitErr)
	require.True(t, limitErr.Compressed)
	require.Equal(t, int64(limit+1), limitErr.Read)
	require.False(t, limitErr.BodyComplete)

	// Only limit+1 bytes (1-byte chunks) were pulled; remaining unknown
	// data was not drained.
	require.Equal(t, limit+1, counter.bytes)

	// Not marked replayable: callers must not believe the body is whole.
	require.Nil(t, req.GetBody)
	require.Equal(t, int64(-1), req.ContentLength)
	partial, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Len(t, partial, int(limit+1))
	require.Equal(t, gz[:limit+1], partial)
}

func TestGzipRequestBodyUnderlyingReadFailure(t *testing.T) {
	t.Parallel()

	plain := []byte(`{"id":1}`)
	gz := mustGzip(t, plain)
	const delivered = 7
	errBoom := errors.New("simulated transport failure")

	body := &failAfterReader{data: gz[:delivered], fail: errBoom}
	options := gzipTestOptions()
	req := newGzipRequest(t, "/things", body, "application/json")
	err := validateGzipRequest(t, req, options)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrRequestBodyRead))
	require.True(t, errors.Is(err, errBoom))
	var readErr *BodyReadError
	require.ErrorAs(t, err, &readErr)

	require.Nil(t, req.GetBody)
	require.Equal(t, int64(-1), req.ContentLength)
	partial, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, gz[:delivered], partial)
}

func TestGzipRequestBodyCorruptVariants(t *testing.T) {
	t.Parallel()

	plain := []byte(`{"id":1}`)
	gz := mustGzip(t, plain)

	cases := []struct {
		name         string
		raw          []byte
		wantSentinel error
		wantInner    error
	}{
		{name: "truncated", raw: gz[:len(gz)-4], wantSentinel: ErrGzipBodyCorrupt, wantInner: io.ErrUnexpectedEOF},
		// Flip a byte of the trailer CRC32: decompression succeeds, then
		// the checksum check must fail deterministically.
		{name: "checksum", raw: flipByte(gz, len(gz)-5), wantSentinel: ErrGzipBodyCorrupt, wantInner: gzip.ErrChecksum},
		{name: "garbage header", raw: []byte{0x01, 0x02, 0x03, 0x04}, wantSentinel: ErrGzipBodyCorrupt, wantInner: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Deliver in 1-byte chunks too.
			req := newGzipRequest(t, "/things", &perByteReader{r: bytes.NewReader(tc.raw)}, "application/json")
			err := validateGzipRequest(t, req, gzipTestOptions())
			require.Error(t, err)
			require.True(t, errors.Is(err, tc.wantSentinel))
			if tc.wantInner != nil {
				require.True(t, errors.Is(err, tc.wantInner), "expected inner error %v, got %v", tc.wantInner, err)
			}
			var gzipErr *GzipBodyError
			require.ErrorAs(t, err, &gzipErr)

			// The complete bytes that were actually delivered can be
			// re-read unchanged.
			require.NotNil(t, req.GetBody)
			again, err := req.GetBody()
			require.NoError(t, err)
			replayed, err := io.ReadAll(again)
			require.NoError(t, err)
			require.Equal(t, tc.raw, replayed)
			require.Equal(t, "gzip", req.Header.Get("Content-Encoding"))
		})
	}
}

func flipByte(b []byte, i int) []byte {
	out := append([]byte(nil), b...)
	out[i] ^= 0xFF
	return out
}

func TestGzipRequestBodyTwoMembersAndTrailingGarbage(t *testing.T) {
	t.Parallel()

	first := mustGzip(t, []byte(`{"id":1}`))
	second := mustGzip(t, []byte(`{"id":2}`))

	cases := []struct {
		name string
		raw  []byte
	}{
		{name: "two members", raw: append(append([]byte{}, first...), second...)},
		{name: "trailing garbage", raw: append(append([]byte{}, first...), 'j', 'u', 'n', 'k')},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := newGzipRequest(t, "/things", bytes.NewReader(tc.raw), "application/json")
			err := validateGzipRequest(t, req, gzipTestOptions())
			require.Error(t, err)
			require.True(t, errors.Is(err, ErrGzipBodyCorrupt))
			var gzipErr *GzipBodyError
			require.ErrorAs(t, err, &gzipErr)
			require.Contains(t, gzipErr.Reason, "trailing")

			// Original (multi-member / garbage) bytes replay verbatim.
			again, err := req.GetBody()
			require.NoError(t, err)
			replayed, err := io.ReadAll(again)
			require.NoError(t, err)
			require.Equal(t, tc.raw, replayed)
		})
	}
}

func TestGzipRequestBodyUnsupportedEncodingRejectedBeforeRead(t *testing.T) {
	t.Parallel()

	gz := mustGzip(t, []byte(`{"id":1}`))

	cases := []struct {
		name    string
		headers map[string][]string
	}{
		{name: "brotli", headers: map[string][]string{"Content-Encoding": {"br"}}},
		{name: "deflate", headers: map[string][]string{"Content-Encoding": {"deflate"}}},
		{name: "x-gzip alias", headers: map[string][]string{"Content-Encoding": {"x-gzip"}}},
		{name: "gzip stack", headers: map[string][]string{"Content-Encoding": {"gzip, gzip"}}},
		{name: "identity then gzip", headers: map[string][]string{"Content-Encoding": {"identity, gzip"}}},
		{name: "gzip then identity", headers: map[string][]string{"Content-Encoding": {"gzip, identity"}}},
		{name: "gzip and br", headers: map[string][]string{"Content-Encoding": {"gzip, br"}}},
		{name: "two header fields", headers: map[string][]string{"Content-Encoding": {"gzip", "gzip"}}},
		{name: "non-json media type", headers: map[string][]string{"Content-Encoding": {"gzip"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			contentType := "application/json"
			if tc.name == "non-json media type" {
				contentType = "text/plain"
			}
			counter := &countingReadCloser{r: bytes.NewReader(gz)}
			req, err := http.NewRequest(http.MethodPost, "/things", counter)
			require.NoError(t, err)
			req.Header.Set("Content-Type", contentType)
			for k, vs := range tc.headers {
				for _, v := range vs {
					req.Header.Add(k, v)
				}
			}

			err = validateGzipRequest(t, req, gzipTestOptions())
			require.Error(t, err)
			require.True(t, errors.Is(err, ErrUnsupportedContentEncoding), "got %v", err)
			var encErr *ContentEncodingError
			require.ErrorAs(t, err, &encErr)

			// Rejected before reading a single body byte.
			require.Equal(t, int64(0), counter.bytes)
			require.Equal(t, int64(0), counter.calls)
		})
	}
}

func TestGzipRequestBodyIdentityFallsThrough(t *testing.T) {
	t.Parallel()

	plain := []byte(`{"id":1}`)

	for _, ce := range []string{"", "identity", "IDENTITY", "  "} {
		t.Run("ce="+ce, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest(http.MethodPost, "/things", bytes.NewReader(plain))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			if ce != "" {
				req.Header.Set("Content-Encoding", ce)
			}
			require.NoError(t, validateGzipRequest(t, req, gzipPassThroughOptions()))
			got, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.Equal(t, plain, got)
		})
	}

	// Uppercase/whitespace-padded gzip token is accepted; the header is
	// preserved verbatim rather than being rewritten or stripped.
	gz := mustGzip(t, plain)
	req, err := http.NewRequest(http.MethodPost, "/things", bytes.NewReader(gz))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", " GZIP ")
	require.NoError(t, validateGzipRequest(t, req, gzipPassThroughOptions()))
	require.Equal(t, " GZIP ", req.Header.Get("Content-Encoding"))
	require.Equal(t, gz, mustReadAll(t, req.Body))
}

func mustReadAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	return b
}

func TestGzipRequestBodyDefaultRewrite(t *testing.T) {
	t.Parallel()

	original := []byte(`{"id":7}`)
	gz := mustGzip(t, original)

	req := newGzipRequest(t, "/things", bytes.NewReader(gz), "application/json")
	require.NoError(t, validateGzipRequest(t, req, gzipTestOptions()))

	// Still gzip downstream, headers consistent, length not stale.
	require.Equal(t, "gzip", req.Header.Get("Content-Encoding"))
	require.Equal(t, "application/json", req.Header.Get("Content-Type"))
	rewritten, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.NotEqual(t, gz, rewritten)
	require.Equal(t, int64(len(rewritten)), req.ContentLength)
	plain := mustGunzip(t, rewritten)
	require.JSONEq(t, `{"id":7,"name":"default-name"}`, string(plain))

	// GetBody agrees with Body.
	again, err := req.GetBody()
	require.NoError(t, err)
	againBytes, err := io.ReadAll(again)
	require.NoError(t, err)
	require.Equal(t, rewritten, againBytes)

	// Repeated validation must not compress a second time.
	require.NoError(t, validateGzipRequest(t, req, gzipTestOptions()))
	twice, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, rewritten, twice)
	plain2 := mustGunzip(t, twice)
	require.JSONEq(t, `{"id":7,"name":"default-name"}`, string(plain2))
	// The decompressed payload must not itself be gzip data.
	badReader, badErr := gzip.NewReader(bytes.NewReader(plain2))
	if badErr == nil {
		_, copyErr := io.ReadAll(badReader)
		require.Error(t, copyErr, "body was compressed twice")
	}
}

func TestGzipRequestBodyNoDefaultChangeKeepsBytes(t *testing.T) {
	t.Parallel()

	original := []byte(`{"id":7,"name":"explicit"}`)
	gz := mustGzip(t, original)

	req := newGzipRequest(t, "/things", bytes.NewReader(gz), "application/json")
	require.NoError(t, validateGzipRequest(t, req, gzipTestOptions()))

	got, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, gz, got)
	require.Equal(t, int64(len(gz)), req.ContentLength)
}

func TestGzipRequestBodyKeepValidatedValue(t *testing.T) {
	t.Parallel()

	original := []byte(`{"id":7}`)
	gz := mustGzip(t, original)

	options := gzipTestOptions()
	options.KeepValidatedValue = true
	req := newGzipRequest(t, "/things", bytes.NewReader(gz), "application/json")
	require.NoError(t, validateGzipRequest(t, req, options))

	v, ok := req.Body.(interface{ ValidatedValue() any })
	require.True(t, ok)
	require.Equal(t, map[string]any{
		"id":   json.Number("7"),
		"name": "default-name",
	}, v.ValidatedValue())

	// Downstream body bytes are still the rewritten gzip payload.
	got, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, "gzip", req.Header.Get("Content-Encoding"))
	require.JSONEq(t, `{"id":7,"name":"default-name"}`, string(mustGunzip(t, got)))
}

func TestGzipRequestBodySchemaFailureReplaysOriginal(t *testing.T) {
	t.Parallel()

	original := []byte(`{"id":"not-an-integer"}`)
	gz := mustGzip(t, original)

	req := newGzipRequest(t, "/things", bytes.NewReader(gz), "application/json")
	err := validateGzipRequest(t, req, gzipTestOptions())
	require.Error(t, err)
	var schemaErr *openapi3.SchemaError
	require.ErrorAs(t, err, &schemaErr)

	again, err := req.GetBody()
	require.NoError(t, err)
	replayed, err := io.ReadAll(again)
	require.NoError(t, err)
	require.Equal(t, gz, replayed)
	require.Equal(t, original, mustGunzip(t, replayed))
	require.Equal(t, "gzip", req.Header.Get("Content-Encoding"))
}

func TestGzipRequestBodyEmptyPayloadRequired(t *testing.T) {
	t.Parallel()

	gz := mustGzip(t, nil)
	req := newGzipRequest(t, "/things", bytes.NewReader(gz), "application/json")
	err := validateGzipRequest(t, req, gzipTestOptions())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidRequired)

	again, err := req.GetBody()
	require.NoError(t, err)
	replayed, err := io.ReadAll(again)
	require.NoError(t, err)
	require.Equal(t, gz, replayed)
}

func TestGzipRequestBodyDisabledByDefault(t *testing.T) {
	t.Parallel()

	// With the option absent, nothing changes: compressed bytes are fed
	// to the JSON decoder as-is and fail to parse, and Content-Encoding is
	// not touched or reinterpreted.
	gz := mustGzip(t, []byte(`{"id":1}`))
	req := newGzipRequest(t, "/things", bytes.NewReader(gz), "application/json")
	err := validateGzipRequest(t, req, &Options{})
	require.Error(t, err)
	var parseErr *ParseError
	require.ErrorAs(t, err, &parseErr)
	require.Equal(t, KindInvalidFormat, parseErr.Kind)
	require.Equal(t, "gzip", req.Header.Get("Content-Encoding"))

	// Body is still replayable through the legacy GetBody restoration.
	require.NotNil(t, req.GetBody)
	again, err := req.GetBody()
	require.NoError(t, err)
	replayed, err := io.ReadAll(again)
	require.NoError(t, err)
	require.Equal(t, gz, replayed)

	// And a plain uncompressed request with the option enabled validates.
	plain := []byte(`{"id":1}`)
	req2, err := http.NewRequest(http.MethodPost, "/things", bytes.NewReader(plain))
	require.NoError(t, err)
	req2.Header.Set("Content-Type", "application/json")
	require.NoError(t, validateGzipRequest(t, req2, gzipTestOptions()))
}

func TestGzipRequestBodyDecompressedReadCountBounded(t *testing.T) {
	t.Parallel()

	// The decompressed buffer must not balloon past the limit: feed a zip
	// bomb and assert the retained output stopped within one chunk of the
	// limit rather than containing the whole expansion.
	plain := []byte(`{"id":1,"data":"` + strings.Repeat("\x00", 1<<20) + `"}`)
	gz := mustGzip(t, plain)

	options := &Options{GzipRequestBody: &GzipRequestBodyOptions{
		MaxCompressedBytes:   16 << 20,
		MaxDecompressedBytes: 4096,
	}}
	req := newGzipRequest(t, "/things", bytes.NewReader(gz), "application/json")
	err := validateGzipRequest(t, req, options)
	require.Error(t, err)
	var limitErr *BodyLimitError
	require.ErrorAs(t, err, &limitErr)
	require.False(t, limitErr.Compressed)
	require.LessOrEqual(t, limitErr.Read, int64(4096)+gzipBodyReadChunkSize)
}
