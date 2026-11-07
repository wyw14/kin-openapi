package openapi3filter

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/getkin/kin-openapi/openapi3"
)

// ----- helpers ------------------------------------------------------------

func mkParam(in, name, style string, explode *bool, schema *openapi3.SchemaRef) *openapi3.Parameter {
	p := &openapi3.Parameter{Name: name, In: in, Schema: schema}
	if style != "" {
		p.Style = style
	}
	if explode != nil {
		p.Explode = explode
	}
	return p
}

func encPathParam(style string, explode *bool, schema *openapi3.SchemaRef) *openapi3.Parameter {
	return mkParam(openapi3.ParameterInPath, "p", style, explode, schema)
}

func eqPairs(want ...[2]string) []EncodedQueryPair {
	out := make([]EncodedQueryPair, len(want))
	for i, kv := range want {
		out[i] = EncodedQueryPair{Key: kv[0], Value: kv[1]}
	}
	return out
}

func requireEncodingError(t *testing.T, err error, kind EncodingErrorKind) *EncodingError {
	t.Helper()
	var ee *EncodingError
	require.Error(t, err)
	require.True(t, errors.As(err, &ee), "expected *EncodingError, got %T: %v", err, err)
	require.Equal(t, kind, ee.Kind, "unexpected error kind: %v", ee)
	return ee
}

// ----- spec examples: the documented style matrix ------------------------

func TestEncodeParameter_SpecExamples(t *testing.T) {
	t.Parallel()

	colorSchema := objectOf("R", integerSchema, "G", integerSchema, "B", integerSchema)
	colorValue := map[string]any{"R": 100, "G": 200, "B": 150}

	type tc struct {
		name       string
		param      *openapi3.Parameter
		value      any
		path       string
		query      []EncodedQueryPair
		header     string
		cookie     string
		onlyTarget string // path/query/header/cookie
	}

	cases := []tc{
		// Object properties are emitted in stable, alphabetical key order.
		// --- path: simple ---
		{"path simple primitive", encPathParam("simple", noExplode, integerSchema), 5, "5", nil, "", "", "path"},
		{"path simple array explode=false", encPathParam("simple", noExplode, stringArraySchema), []any{"blue", "black", "brown"}, "blue,black,brown", nil, "", "", "path"},
		{"path simple array explode=true", encPathParam("simple", explode, stringArraySchema), []any{"blue", "black", "brown"}, "blue,black,brown", nil, "", "", "path"},
		{"path simple object explode=false", encPathParam("simple", noExplode, colorSchema), colorValue, "B,150,G,200,R,100", nil, "", "", "path"},
		{"path simple object explode=true", encPathParam("simple", explode, colorSchema), colorValue, "B=150,G=200,R=100", nil, "", "", "path"},
		// --- path: label ---
		{"path label primitive", encPathParam("label", noExplode, integerSchema), 5, ".5", nil, "", "", "path"},
		{"path label array explode=false", encPathParam("label", noExplode, stringArraySchema), []any{"blue", "black", "brown"}, ".blue,black,brown", nil, "", "", "path"},
		{"path label array explode=true", encPathParam("label", explode, stringArraySchema), []any{"blue", "black", "brown"}, ".blue.black.brown", nil, "", "", "path"},
		{"path label object explode=false", encPathParam("label", noExplode, colorSchema), colorValue, ".B,150,G,200,R,100", nil, "", "", "path"},
		{"path label object explode=true", encPathParam("label", explode, colorSchema), colorValue, ".B=150.G=200.R=100", nil, "", "", "path"},
		// --- path: matrix (parameter named "id" like the spec table) ---
		{"path matrix primitive", mkParam("path", "id", "matrix", noExplode, integerSchema), 5, ";id=5", nil, "", "", "path"},
		{"path matrix array explode=false", mkParam("path", "id", "matrix", noExplode, stringArraySchema), []any{"blue", "black", "brown"}, ";id=blue,black,brown", nil, "", "", "path"},
		{"path matrix array explode=true", mkParam("path", "id", "matrix", explode, stringArraySchema), []any{"blue", "black", "brown"}, ";id=blue;id=black;id=brown", nil, "", "", "path"},
		{"path matrix object explode=false", mkParam("path", "id", "matrix", noExplode, colorSchema), colorValue, ";id=B,150,G,200,R,100", nil, "", "", "path"},
		{"path matrix object explode=true", mkParam("path", "id", "matrix", explode, colorSchema), colorValue, ";B=150;G=200;R=100", nil, "", "", "path"},

		// --- query: form ---
		{"query form primitive", mkParam("query", "role", "form", noExplode, stringSchema), "admin", "", eqPairs([2]string{"role", "admin"}), "", "", "query"},
		{"query form array explode=false", mkParam("query", "color", "form", noExplode, stringArraySchema), []any{"blue", "black", "brown"}, "",
			eqPairs([2]string{"color", "blue,black,brown"}), "", "", "query"},
		{"query form array explode=true", mkParam("query", "color", "form", explode, stringArraySchema), []any{"blue", "black", "brown"}, "",
			eqPairs([2]string{"color", "blue"}, [2]string{"color", "black"}, [2]string{"color", "brown"}), "", "", "query"},
		{"query form object explode=false", mkParam("query", "color", "form", noExplode, colorSchema), colorValue, "",
			eqPairs([2]string{"color", "B,150,G,200,R,100"}), "", "", "query"},
		{"query form object explode=true", mkParam("query", "color", "form", explode, colorSchema), colorValue, "",
			eqPairs([2]string{"B", "150"}, [2]string{"G", "200"}, [2]string{"R", "100"}), "", "", "query"},
		// --- query: spaceDelimited / pipeDelimited ---
		{"query spaceDelimited array explode=false", mkParam("query", "color", "spaceDelimited", noExplode, stringArraySchema), []any{"blue", "black", "brown"}, "",
			eqPairs([2]string{"color", "blue%20black%20brown"}), "", "", "query"},
		{"query spaceDelimited array explode=true", mkParam("query", "color", "spaceDelimited", explode, stringArraySchema), []any{"blue", "black", "brown"}, "",
			eqPairs([2]string{"color", "blue"}, [2]string{"color", "black"}, [2]string{"color", "brown"}), "", "", "query"},
		{"query pipeDelimited array explode=false", mkParam("query", "color", "pipeDelimited", noExplode, stringArraySchema), []any{"blue", "black", "brown"}, "",
			eqPairs([2]string{"color", "blue|black|brown"}), "", "", "query"},
		{"query pipeDelimited array explode=true", mkParam("query", "color", "pipeDelimited", explode, stringArraySchema), []any{"blue", "black", "brown"}, "",
			eqPairs([2]string{"color", "blue"}, [2]string{"color", "black"}, [2]string{"color", "brown"}), "", "", "query"},
		// --- query: deepObject ---
		{"query deepObject object", mkParam("query", "color", "deepObject", explode, colorSchema), colorValue, "",
			eqPairs([2]string{"color[B]", "150"}, [2]string{"color[G]", "200"}, [2]string{"color[R]", "100"}), "", "", "query"},

		// --- header: simple ---
		{"header primitive", mkParam("header", "X-MyHeader", "simple", noExplode, integerSchema), 5, "", nil, "5", "", "header"},
		{"header array", mkParam("header", "X-MyHeader", "simple", noExplode, stringArraySchema), []any{"3", "4", "5"}, "", nil, "3,4,5", "", "header"},
		{"header object explode=false", mkParam("header", "X-MyHeader", "simple", noExplode, colorSchema), colorValue, "", nil, "B,150,G,200,R,100", "", "header"},
		{"header object explode=true", mkParam("header", "X-MyHeader", "simple", explode, colorSchema), colorValue, "", nil, "B=150,G=200,R=100", "", "header"},

		// --- cookie: form (non-exploded only) ---
		{"cookie primitive", mkParam("cookie", "id", "form", noExplode, integerSchema), 5, "", nil, "", "5", "cookie"},
		{"cookie array", mkParam("cookie", "id", "form", noExplode, integerArraySchema), []any{1, 2, 3}, "", nil, "", "1,2,3", "cookie"},
		{"cookie object", mkParam("cookie", "id", "form", noExplode, objectOf("role", stringSchema, "firstName", stringSchema)), map[string]any{"role": "admin", "firstName": "Alex"}, "", nil, "", "firstName,Alex,role,admin", "cookie"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			enc, err := EncodeParameter(tc.param, tc.value)
			require.NoError(t, err)
			switch tc.onlyTarget {
			case "path":
				require.NotNil(t, enc.Path)
				require.Equal(t, tc.path, enc.Path.Fragment)
				require.Nil(t, enc.Query)
				require.Nil(t, enc.Header)
				require.Nil(t, enc.Cookie)
			case "query":
				require.Equal(t, tc.query, enc.Query)
				require.Nil(t, enc.Path)
			case "header":
				require.NotNil(t, enc.Header)
				require.Equal(t, tc.header, enc.Header.Value)
			case "cookie":
				require.NotNil(t, enc.Cookie)
				require.Equal(t, tc.cookie, enc.Cookie.Value)
			}
		})
	}
}

// ----- property order, array order, false/0/"" are not missing -----------

func TestEncodeParameter_OrderAndZeroValues(t *testing.T) {
	t.Parallel()

	schema := objectOf("active", booleanSchema, "n", integerSchema, "s", stringSchema)
	value := map[string]any{"active": false, "n": 0, "s": ""}

	// Non-exploded form keeps the stable, sorted property order.
	enc, err := EncodeParameter(mkParam("query", "p", "form", noExplode, schema), value)
	require.NoError(t, err)
	require.Equal(t, eqPairs([2]string{"p", "active,false,n,0,s,"}), enc.Query)

	// Exploded form emits all three keys; zero values are present.
	enc, err = EncodeParameter(mkParam("query", "p", "form", explode, schema), value)
	require.NoError(t, err)
	require.Equal(t, eqPairs(
		[2]string{"active", "false"},
		[2]string{"n", "0"},
		[2]string{"s", ""},
	), enc.Query)

	// Array order, including repeated and zero-ish elements.
	enc, err = EncodeParameter(mkParam("query", "p", "form", explode, stringArraySchema),
		[]any{"a", "", "0", "false", "a"})
	require.NoError(t, err)
	require.Equal(t, eqPairs(
		[2]string{"p", "a"}, [2]string{"p", ""}, [2]string{"p", "0"},
		[2]string{"p", "false"}, [2]string{"p", "a"},
	), enc.Query)
}

// ----- empty arrays and objects ------------------------------------------

func TestEncodeParameter_EmptyContainers(t *testing.T) {
	t.Parallel()

	checkPath := func(t *testing.T, style string, explode *bool, shape string, want string) {
		t.Helper()
		schema := stringArraySchema
		var value any = []any{}
		if shape == "object" {
			schema = objectSchema
			value = map[string]any{}
		}
		enc, err := EncodeParameter(encPathParam(style, explode, schema), value)
		require.NoError(t, err)
		require.Equal(t, want, enc.Path.Fragment)
	}

	checkPath(t, "simple", noExplode, "array", "")
	checkPath(t, "simple", explode, "array", "")
	checkPath(t, "simple", noExplode, "object", "")
	checkPath(t, "simple", explode, "object", "")
	checkPath(t, "label", noExplode, "array", ".")
	checkPath(t, "label", explode, "array", ".")
	checkPath(t, "label", noExplode, "object", ".")
	checkPath(t, "label", explode, "object", ".")
	checkPath(t, "matrix", noExplode, "array", ";p=")
	checkPath(t, "matrix", explode, "array", ";p=")
	checkPath(t, "matrix", noExplode, "object", ";p=")
	checkPath(t, "matrix", explode, "object", ";")

	// Empty primitive string renders an empty value everywhere.
	enc, err := EncodeParameter(encPathParam("simple", noExplode, stringSchema), "")
	require.NoError(t, err)
	require.Equal(t, "", enc.Path.Fragment)
	enc, err = EncodeParameter(encPathParam("label", noExplode, stringSchema), "")
	require.NoError(t, err)
	require.Equal(t, ".", enc.Path.Fragment)
	enc, err = EncodeParameter(encPathParam("matrix", noExplode, stringSchema), "")
	require.NoError(t, err)
	require.Equal(t, ";p=", enc.Path.Fragment)

	// Query: non-exploded styles emit one empty pair; exploded styles have
	// no defined wire token for an empty container and emit no pairs.
	for _, style := range []string{"form", "spaceDelimited", "pipeDelimited"} {
		enc, err := EncodeParameter(mkParam("query", "p", style, noExplode, stringArraySchema), []any{})
		require.NoError(t, err)
		require.Equal(t, eqPairs([2]string{"p", ""}), enc.Query, style)
	}
	enc, err = EncodeParameter(mkParam("query", "p", "form", explode, stringArraySchema), []any{})
	require.NoError(t, err)
	require.Empty(t, enc.Query)
	enc, err = EncodeParameter(mkParam("query", "p", "form", noExplode, objectSchema), map[string]any{})
	require.NoError(t, err)
	require.Equal(t, eqPairs([2]string{"p", ""}), enc.Query)
	enc, err = EncodeParameter(mkParam("query", "p", "form", explode, objectSchema), map[string]any{})
	require.NoError(t, err)
	require.Empty(t, enc.Query)
	enc, err = EncodeParameter(mkParam("query", "p", "deepObject", explode, objectSchema), map[string]any{})
	require.NoError(t, err)
	require.Empty(t, enc.Query)
	enc, err = EncodeParameter(mkParam("query", "p", "form", noExplode, stringSchema), "")
	require.NoError(t, err)
	require.Equal(t, eqPairs([2]string{"p", ""}), enc.Query)

	// Header and cookie: empty containers render an empty raw value.
	for _, ex := range []*bool{noExplode, explode} {
		enc, err := EncodeParameter(mkParam("header", "X-P", "simple", ex, stringArraySchema), []any{})
		require.NoError(t, err)
		require.Equal(t, "", enc.Header.Value)
		enc, err = EncodeParameter(mkParam("header", "X-P", "simple", ex, objectSchema), map[string]any{})
		require.NoError(t, err)
		require.Equal(t, "", enc.Header.Value)
	}
	enc, err = EncodeParameter(mkParam("cookie", "p", "form", noExplode, stringArraySchema), []any{})
	require.NoError(t, err)
	require.Equal(t, "", enc.Cookie.Value)
	enc, err = EncodeParameter(mkParam("cookie", "p", "form", noExplode, objectSchema), map[string]any{})
	require.NoError(t, err)
	require.Equal(t, "", enc.Cookie.Value)
}

// ----- escaping happens before structuring -------------------------------

func TestEncodeParameter_EscapesBeforeStructuring(t *testing.T) {
	t.Parallel()

	// Path: delimiters in data are percent-encoded, the structural
	// delimiters stay raw.
	enc, err := EncodeParameter(encPathParam("simple", noExplode, stringArraySchema), []any{"a,b", "c;d"})
	require.NoError(t, err)
	require.Equal(t, "a%2Cb,c%3Bd", enc.Path.Fragment)

	enc, err = EncodeParameter(encPathParam("label", explode, stringArraySchema), []any{"a.b", "c"})
	require.NoError(t, err)
	require.Equal(t, ".a%2Eb.c", enc.Path.Fragment)

	enc, err = EncodeParameter(mkParam("path", "p", "matrix", explode, stringArraySchema), []any{"a=b", "c"})
	require.NoError(t, err)
	require.Equal(t, ";p=a%3Db;p=c", enc.Path.Fragment)

	// Slashes in data never open a new path segment.
	enc, err = EncodeParameter(encPathParam("simple", noExplode, stringSchema), "a/b")
	require.NoError(t, err)
	require.Equal(t, "a%2Fb", enc.Path.Fragment)

	// Existing percent signs are encoded once, never decoded first.
	enc, err = EncodeParameter(encPathParam("simple", noExplode, stringSchema), "100%")
	require.NoError(t, err)
	require.Equal(t, "100%25", enc.Path.Fragment)
	raw, err := url.PathUnescape(enc.Path.Fragment)
	require.NoError(t, err)
	require.Equal(t, "100%", raw)

	// Unicode is UTF-8 percent-encoded.
	enc, err = EncodeParameter(encPathParam("simple", noExplode, stringSchema), "café")
	require.NoError(t, err)
	require.Equal(t, "caf%C3%A9", enc.Path.Fragment)

	// Query non-exploded form: data comma encoded, structural comma raw.
	enc, err = EncodeParameter(mkParam("query", "p", "form", noExplode, stringArraySchema), []any{"a,b", "c"})
	require.NoError(t, err)
	require.Equal(t, "a%2Cb,c", enc.Query[0].Value)

	// Data pipe / space are encoded in their own styles.
	enc, err = EncodeParameter(mkParam("query", "p", "pipeDelimited", noExplode, stringArraySchema), []any{"a|b", "c"})
	require.NoError(t, err)
	require.Equal(t, "a%7Cb|c", enc.Query[0].Value)
	enc, err = EncodeParameter(mkParam("query", "p", "spaceDelimited", noExplode, stringArraySchema), []any{"a b", "c"})
	require.NoError(t, err)
	require.Equal(t, "a+b%20c", enc.Query[0].Value)

	// Space -> '+', literal '+' -> %2B.
	enc, err = EncodeParameter(mkParam("query", "p", "form", noExplode, stringSchema), "a b+c")
	require.NoError(t, err)
	require.Equal(t, "a+b%2Bc", enc.Query[0].Value)

	// Existing percent in query values is preserved through one encoding.
	enc, err = EncodeParameter(mkParam("query", "p", "form", noExplode, stringSchema), "100%")
	require.NoError(t, err)
	require.Equal(t, "100%25", enc.Query[0].Value)

	// Slash is reserved and encoded by default, but allowReserved keeps it raw.
	enc, err = EncodeParameter(mkParam("query", "p", "form", noExplode, stringSchema), "a/b,c")
	require.NoError(t, err)
	require.Equal(t, "a%2Fb%2Cc", enc.Query[0].Value)
	ar := mkParam("query", "p", "form", noExplode, stringSchema)
	ar.AllowReserved = true
	enc, err = EncodeParameter(ar, "a/b,c")
	require.NoError(t, err)
	require.Equal(t, "a/b,c", enc.Query[0].Value)

	// Keys with delimiter characters are escaped so they cannot break the
	// query structure.
	enc, err = EncodeParameter(mkParam("query", "p", "form", explode, objectSchema),
		map[string]any{"a,b": "v", "id": "x"})
	require.NoError(t, err)
	require.Equal(t, "a%2Cb", enc.Query[0].Key)
	enc, err = EncodeParameter(mkParam("query", "p", "deepObject", explode, objectSchema),
		map[string]any{"a[b]": "v", "id": "x"})
	require.NoError(t, err)
	require.Equal(t, "p[a%5Bb%5D]", enc.Query[0].Key)

	// Header simple primitive keeps reserved punctuation verbatim but no
	// percent-encoding is applied.
	enc, err = EncodeParameter(mkParam("header", "X-P", "simple", noExplode, stringSchema), "a,b;c=d/e")
	require.NoError(t, err)
	require.Equal(t, "a,b;c=d/e", enc.Header.Value)
}

// ----- illegal header/cookie bytes ---------------------------------------

func TestEncodeParameter_IllegalHeaderCookie(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"x\ny", "x\ry", "x\x00y", "x\x7fy"} {
		_, err := EncodeParameter(mkParam("header", "X-P", "simple", noExplode, stringSchema), bad)
		requireEncodingError(t, err, EncodingErrorIllegal)
	}

	// A comma in an array item cannot be escaped in header simple style.
	_, err := EncodeParameter(mkParam("header", "X-P", "simple", noExplode, stringArraySchema), []any{"a,b", "c"})
	requireEncodingError(t, err, EncodingErrorUnsupported)

	// In exploded objects ',' and '=' are structural in keys and values.
	_, err = EncodeParameter(mkParam("header", "X-P", "simple", explode, objectOf("k", stringSchema)),
		map[string]any{"k": "a,b"})
	requireEncodingError(t, err, EncodingErrorUnsupported)
	_, err = EncodeParameter(mkParam("header", "X-P", "simple", explode, objectOf("k", stringSchema)),
		map[string]any{"k": "a=b"})
	requireEncodingError(t, err, EncodingErrorUnsupported)

	// Invalid header name.
	_, err = EncodeParameter(mkParam("header", "x y", "simple", noExplode, stringSchema), "v")
	requireEncodingError(t, err, EncodingErrorUnsupported)

	// Cookie legality per RFC 6265 cookie-octet.
	_, err = EncodeParameter(mkParam("cookie", "p", "form", noExplode, stringSchema), "a b")
	requireEncodingError(t, err, EncodingErrorIllegal)
	_, err = EncodeParameter(mkParam("cookie", "p", "form", noExplode, stringSchema), "a\nb")
	requireEncodingError(t, err, EncodingErrorIllegal)
	_, err = EncodeParameter(mkParam("cookie", "p", "form", noExplode, stringSchema), "café")
	requireEncodingError(t, err, EncodingErrorIllegal)
	_, err = EncodeParameter(mkParam("cookie", "p", "form", noExplode, stringSchema), "a;b")
	requireEncodingError(t, err, EncodingErrorUnsupported)
	_, err = EncodeParameter(mkParam("cookie", "p", "form", noExplode, stringArraySchema), []any{"a,b"})
	requireEncodingError(t, err, EncodingErrorUnsupported)
	_, err = EncodeParameter(mkParam("cookie", "p", "form", noExplode, stringSchema), `"quoted"`)
	requireEncodingError(t, err, EncodingErrorIllegal)

	// '%' is a legal cookie-octet and is passed through untouched.
	enc, err := EncodeParameter(mkParam("cookie", "p", "form", noExplode, stringSchema), "50%")
	require.NoError(t, err)
	require.Equal(t, "50%", enc.Cookie.Value)
}

// ----- unsupported combinations ------------------------------------------

func TestEncodeParameter_Unsupported(t *testing.T) {
	t.Parallel()

	emptySchema := &openapi3.SchemaRef{Value: &openapi3.Schema{Nullable: true}}

	// null has no representation in any location/style, even when the
	// schema explicitly permits null.
	for _, in := range []string{"path", "query", "header", "cookie"} {
		style := "simple"
		explode := noExplode
		if in == "query" || in == "cookie" {
			style, explode = "form", noExplode
		}
		_, err := EncodeParameter(mkParam(in, "p", style, explode, emptySchema), nil)
		requireEncodingError(t, err, EncodingErrorUnsupported)
	}

	// null inside an array / object is rejected even if the schema tolerates it.
	looseArray := &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:  &openapi3.Types{"array"},
		Items: emptySchema,
	}}
	_, err := EncodeParameter(mkParam("query", "p", "form", noExplode, looseArray), []any{"a", nil})
	requireEncodingError(t, err, EncodingErrorUnsupported)

	looseObject := &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:                 &openapi3.Types{"object"},
		AdditionalProperties: openapi3.AdditionalProperties{Schema: emptySchema},
	}}
	_, err = EncodeParameter(mkParam("query", "p", "form", noExplode, looseObject), map[string]any{"k": nil})
	requireEncodingError(t, err, EncodingErrorUnsupported)

	// deepObject: only flat objects with explode=true.
	_, err = EncodeParameter(mkParam("query", "p", "deepObject", explode, stringSchema), "x")
	requireEncodingError(t, err, EncodingErrorUnsupported)
	_, err = EncodeParameter(mkParam("query", "p", "deepObject", explode, stringArraySchema), []any{"a", "b"})
	requireEncodingError(t, err, EncodingErrorUnsupported)
	_, err = EncodeParameter(mkParam("query", "p", "deepObject", noExplode, objectSchema), map[string]any{"id": "x"})
	requireEncodingError(t, err, EncodingErrorUnsupported)
	nestedDeep := objectOf("items", stringArraySchema)
	_, err = EncodeParameter(mkParam("query", "p", "deepObject", explode, nestedDeep),
		map[string]any{"items": []any{"a", "b"}})
	requireEncodingError(t, err, EncodingErrorUnsupported)
	nestedObjDeep := objectOf("obj", objectOf("inner", stringSchema))
	_, err = EncodeParameter(mkParam("query", "p", "deepObject", explode, nestedObjDeep),
		map[string]any{"obj": map[string]any{"inner": "v"}})
	requireEncodingError(t, err, EncodingErrorUnsupported)

	// space/pipe styles are defined for arrays only.
	_, err = EncodeParameter(mkParam("query", "p", "spaceDelimited", noExplode, stringSchema), "x")
	requireEncodingError(t, err, EncodingErrorUnsupported)
	_, err = EncodeParameter(mkParam("query", "p", "pipeDelimited", noExplode, objectSchema), map[string]any{"id": "x"})
	requireEncodingError(t, err, EncodingErrorUnsupported)

	// Cookies: explode for arrays/objects is not defined.
	_, err = EncodeParameter(mkParam("cookie", "p", "form", explode, stringArraySchema), []any{"a"})
	requireEncodingError(t, err, EncodingErrorUnsupported)
	_, err = EncodeParameter(mkParam("cookie", "p", "form", explode, objectSchema), map[string]any{"id": "x"})
	requireEncodingError(t, err, EncodingErrorUnsupported)

	// Wrong style for the location.
	_, err = EncodeParameter(mkParam("path", "p", "form", noExplode, stringSchema), "x")
	requireEncodingError(t, err, EncodingErrorUnsupported)
	_, err = EncodeParameter(mkParam("query", "p", "simple", noExplode, stringSchema), "x")
	requireEncodingError(t, err, EncodingErrorUnsupported)
	_, err = EncodeParameter(mkParam("header", "X-P", "form", noExplode, stringSchema), "x")
	requireEncodingError(t, err, EncodingErrorUnsupported)
	_, err = EncodeParameter(mkParam("cookie", "p", "simple", noExplode, stringSchema), "x")
	requireEncodingError(t, err, EncodingErrorUnsupported)
	_, err = EncodeParameter(mkParam("query", "p", "frobnicate", noExplode, stringSchema), "x")
	requireEncodingError(t, err, EncodingErrorUnsupported)
	_, err = EncodeParameter(mkParam("event", "p", "", nil, stringSchema), "x")
	requireEncodingError(t, err, EncodingErrorUnsupported)

	// content parameters and schema-less parameters are not supported.
	contentParam := &openapi3.Parameter{
		Name: "p", In: "query",
		Content: openapi3.Content{"application/json": &openapi3.MediaType{Schema: stringSchema}},
	}
	_, err = EncodeParameter(contentParam, "x")
	requireEncodingError(t, err, EncodingErrorUnsupported)
	_, err = EncodeParameter(&openapi3.Parameter{Name: "p", In: "query"}, "x")
	requireEncodingError(t, err, EncodingErrorUnsupported)
	_, err = EncodeParameter(nil, "x")
	requireEncodingError(t, err, EncodingErrorUnsupported)

	// NaN cannot be serialized: refused rather than rendered as "NaN".
	_, err = EncodeParameter(mkParam("query", "p", "form", noExplode, numberSchema), math.NaN())
	requireEncodingError(t, err, EncodingErrorInvalid)
	_, err = EncodeParameter(mkParam("query", "p", "form", noExplode, numberSchema), math.Inf(1))
	require.Error(t, err)
}

// ----- schema validation happens first, no default injection -------------

func TestEncodeParameter_SchemaValidation(t *testing.T) {
	t.Parallel()

	_, err := EncodeParameter(mkParam("query", "p", "form", noExplode, integerSchema), "foo")
	ee := requireEncodingError(t, err, EncodingErrorInvalid)
	var se *openapi3.SchemaError
	require.True(t, errors.As(err, &se), "cause should be a schema error: %v", err)
	require.True(t, errors.Is(err, ErrInvalidValue))
	require.NotNil(t, ee.Cause)

	enumSchema := &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type: &openapi3.Types{"string"}, Enum: []any{"a", "b"},
	}}
	_, err = EncodeParameter(mkParam("query", "p", "form", noExplode, enumSchema), "c")
	requireEncodingError(t, err, EncodingErrorInvalid)

	// A schema default is never used in place of a missing/null value, even
	// when the schema would permit null.
	withDefault := &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type: &openapi3.Types{"string"}, Nullable: true, Default: "default-value",
	}}
	_, err = EncodeParameter(mkParam("query", "p", "form", noExplode, withDefault), nil)
	requireEncodingError(t, err, EncodingErrorUnsupported)

	// Unsupported style on a value that itself is schema-valid: the style
	// error must not be masked by a fallback to fmt.Sprint.
	_, err = EncodeParameter(mkParam("path", "p", "deepObject", explode, stringSchema), "x")
	requireEncodingError(t, err, EncodingErrorUnsupported)
}

// ----- round trip through the existing decoder ---------------------------

func encNumeric(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func encLooseEqual(a, b any) bool {
	if an, ok := encNumeric(a); ok {
		bn, ok := encNumeric(b)
		return ok && an == bn
	}
	switch av := a.(type) {
	case nil:
		return b == nil
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !encLooseEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			if !encLooseEqual(v, bv[k]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}

func decodeEncoded(t *testing.T, param *openapi3.Parameter, enc *EncodedParameter) (any, bool, error) {
	t.Helper()
	sm, err := param.SerializationMethod()
	require.NoError(t, err)

	var dec valueDecoder
	switch param.In {
	case openapi3.ParameterInPath:
		raw, uerr := url.PathUnescape(enc.Path.Fragment)
		require.NoError(t, uerr)
		dec = &pathParamDecoder{pathParams: map[string]string{param.Name: raw}}
	case openapi3.ParameterInQuery:
		parts := make([]string, 0, len(enc.Query))
		for _, p := range enc.Query {
			parts = append(parts, p.Key+"="+p.Value)
		}
		values, perr := url.ParseQuery(strings.Join(parts, "&"))
		require.NoError(t, perr)
		dec = &urlValuesDecoder{values: values}
	case openapi3.ParameterInHeader:
		dec = &headerParamDecoder{header: http.Header{
			http.CanonicalHeaderKey(param.Name): {enc.Header.Value},
		}}
	case openapi3.ParameterInCookie:
		req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
		req.AddCookie(&http.Cookie{Name: param.Name, Value: enc.Cookie.Value})
		dec = &cookieParamDecoder{req: req}
	}
	return decodeValue(dec, param.Name, sm, param.Schema, false)
}

func TestEncodeParameter_RoundTrip(t *testing.T) {
	t.Parallel()

	colorSchema := objectOf("R", integerSchema, "G", integerSchema, "B", integerSchema)
	colorValue := map[string]any{"R": 100, "G": 200, "B": 150}
	idName := objectOf("id", stringSchema, "name", stringSchema)
	idNameValue := map[string]any{"id": "foo", "name": "bar"}
	mixedSchema := objectOf("active", booleanSchema, "n", numberSchema)
	mixedValue := map[string]any{"active": true, "n": 1.5}

	type rtCase struct {
		name  string
		param *openapi3.Parameter
		value any
	}
	var cases []rtCase
	add := func(name, in, style string, explode *bool, schema *openapi3.SchemaRef, value any) {
		cases = append(cases, rtCase{name, mkParam(in, "p", style, explode, schema), value})
	}

	// Primitives across all locations and styles.
	for _, v := range []any{"abc", 42, 1.5, true} {
		schema := stringSchema
		switch v.(type) {
		case int:
			schema = integerSchema
		case float64:
			schema = numberSchema
		case bool:
			schema = booleanSchema
		}
		add("path simple primitive", "path", "simple", noExplode, schema, v)
		add("path label primitive", "path", "label", noExplode, schema, v)
		add("path matrix primitive", "path", "matrix", noExplode, schema, v)
		add("query form primitive", "query", "form", noExplode, schema, v)
		add("header simple primitive", "header", "simple", noExplode, schema, v)
		add("cookie form primitive", "cookie", "form", noExplode, schema, v)
	}

	// Arrays.
	for _, style := range []string{"simple", "label", "matrix"} {
		for _, ex := range []*bool{noExplode, explode} {
			add("path "+style+" string array", "path", style, ex, stringArraySchema, []any{"blue", "black", "brown"})
			add("path "+style+" int array", "path", style, ex, integerArraySchema, []any{1, 2, 3})
		}
	}
	for _, style := range []string{"form", "spaceDelimited", "pipeDelimited"} {
		for _, ex := range []*bool{noExplode, explode} {
			add("query "+style+" string array", "query", style, ex, stringArraySchema, []any{"blue", "black", "brown"})
			add("query "+style+" int array", "query", style, ex, integerArraySchema, []any{1, 2, 3})
		}
	}
	add("header string array", "header", "simple", noExplode, stringArraySchema, []any{"3", "4", "5"})
	add("header int array", "header", "simple", noExplode, integerArraySchema, []any{3, 4, 5})
	add("cookie string array", "cookie", "form", noExplode, stringArraySchema, []any{"a", "b", "c"})

	// Flat objects.
	for _, style := range []string{"simple", "label", "matrix"} {
		for _, ex := range []*bool{noExplode, explode} {
			add("path "+style+" string object", "path", style, ex, idName, idNameValue)
			add("path "+style+" int object", "path", style, ex, colorSchema, colorValue)
		}
	}
	add("query form object false", "query", "form", noExplode, idName, idNameValue)
	add("query form object true", "query", "form", explode, idName, idNameValue)
	add("query deepObject", "query", "deepObject", explode, colorSchema, colorValue)
	add("query deepObject mixed", "query", "deepObject", explode, mixedSchema, mixedValue)
	add("header object false", "header", "simple", noExplode, idName, idNameValue)
	add("header object true", "header", "simple", explode, idName, idNameValue)
	add("cookie object", "cookie", "form", noExplode, idName, idNameValue)

	// Reserved/unicode characters in atomic values.
	add("query reserved primitive explode", "query", "form", explode, stringSchema, "a,b;c=d/e")
	add("query space and plus primitive", "query", "form", noExplode, stringSchema, "a b+c")
	add("query percent primitive", "query", "form", noExplode, stringSchema, "100%")
	add("query unicode primitive", "query", "form", noExplode, stringSchema, "café")
	add("path reserved primitive", "path", "simple", noExplode, stringSchema, "a/b,c;d=e.g")
	add("header punctuation primitive", "header", "simple", noExplode, stringSchema, "a,b;c=d")
	add("cookie slash primitive", "cookie", "form", noExplode, stringSchema, "a/b")
	add("cookie equals primitive", "cookie", "form", noExplode, stringSchema, "a=b")
	add("cookie percent primitive", "cookie", "form", noExplode, stringSchema, "50%")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			enc, err := EncodeParameter(tc.param, tc.value)
			require.NoError(t, err)
			got, found, derr := decodeEncoded(t, tc.param, enc)
			require.NoError(t, derr, "decoder rejected the encoding")
			require.True(t, found, "decoder could not find the parameter")
			require.True(t, encLooseEqual(tc.value, got), "round trip mismatch\nencoded: %#v\ngot: %#v\nwant: %#v", enc, got, tc.value)
		})
	}
}

// ----- multiple parameters: order, duplicates and ownership --------------

func TestEncodeParameters_Merge(t *testing.T) {
	t.Parallel()

	pathP := encPathParam("matrix", noExplode, stringSchema)
	queryA := mkParam("query", "a", "form", explode, stringArraySchema)
	queryB := mkParam("query", "b", "form", noExplode, stringSchema)
	headerP := mkParam("header", "X-H", "simple", noExplode, stringSchema)
	cookieP := mkParam("cookie", "c", "form", noExplode, stringSchema)

	got, err := EncodeParameters(
		ParameterValue{pathP, "x"},
		ParameterValue{queryA, []any{"1", "2", "1"}},
		ParameterValue{queryB, "y"},
		ParameterValue{headerP, "h"},
		ParameterValue{cookieP, "k"},
	)
	require.NoError(t, err)
	require.Equal(t, []EncodedPathParameter{{Name: "p", Fragment: ";p=x"}}, got.Path)
	require.Equal(t, eqPairs(
		[2]string{"a", "1"}, [2]string{"a", "2"}, [2]string{"a", "1"}, [2]string{"b", "y"},
	), got.Query)
	require.Equal(t, []EncodedHeaderParameter{{Name: "X-H", Value: "h"}}, got.Header)
	require.Equal(t, []EncodedCookieParameter{{Name: "c", Value: "k"}}, got.Cookie)
}

func TestEncodeParameters_OwnershipConflict(t *testing.T) {
	t.Parallel()

	// An exploded form object owns each property key; "id" collides with a
	// standalone parameter of the same name.
	filter := mkParam("query", "filter", "form", explode, objectSchema)
	id := mkParam("query", "id", "form", noExplode, stringSchema)
	_, err := EncodeParameters(
		ParameterValue{filter, map[string]any{"id": "1", "name": "x"}},
		ParameterValue{id, "2"},
	)
	ee := requireEncodingError(t, err, EncodingErrorConflict)
	require.Equal(t, "id", ee.Key)
	require.Equal(t, "id", ee.Parameter.Name) // the later claim is rejected
	require.Equal(t, "filter", ee.OtherParameter.Name)
	require.True(t, errors.Is(err, ErrKeyOwnershipConflict))

	// Reverse order is detected too.
	_, err = EncodeParameters(
		ParameterValue{id, "2"},
		ParameterValue{filter, map[string]any{"id": "1", "name": "x"}},
	)
	ee = requireEncodingError(t, err, EncodingErrorConflict)
	require.Equal(t, "id", ee.Key)
	require.Equal(t, "filter", ee.Parameter.Name)
	require.Equal(t, "id", ee.OtherParameter.Name)

	// deepObject owns "filter[id]" at the logical level; a parameter with
	// the literal bracketed name collides even though its wire key is
	// percent-encoded differently.
	deep := mkParam("query", "filter", "deepObject", explode, objectSchema)
	literalBracket := mkParam("query", "filter[id]", "form", noExplode, stringSchema)
	_, err = EncodeParameters(
		ParameterValue{deep, map[string]any{"id": "1"}},
		ParameterValue{literalBracket, "2"},
	)
	ee = requireEncodingError(t, err, EncodingErrorConflict)
	require.Equal(t, "filter[id]", ee.Key)

	// Repeated keys owned by the SAME exploded parameter are fine.
	dupA := mkParam("query", "a", "form", explode, stringArraySchema)
	_, err = EncodeParameters(ParameterValue{dupA, []any{"1", "1"}})
	require.NoError(t, err)

	// Non-overlapping deepObject and standalone keys coexist.
	_, err = EncodeParameters(
		ParameterValue{deep, map[string]any{"id": "1"}},
		ParameterValue{mkParam("query", "name", "form", noExplode, stringSchema), "x"},
	)
	require.NoError(t, err)
}

// ----- inputs are not mutated --------------------------------------------

func TestEncodeParameter_NoMutation(t *testing.T) {
	t.Parallel()

	param := mkParam("query", "p", "form", explode, objectSchema)
	value := map[string]any{"id": "x", "name": "y"}

	paramCopy := *param
	valueCopy := map[string]any{"id": "x", "name": "y"}

	_, err := EncodeParameter(param, value)
	require.NoError(t, err)
	require.Equal(t, paramCopy, *param)
	require.Equal(t, valueCopy, value)
}

// ----- the encoder never touches a request ------------------------------

func TestEncodeParameter_RequestFree(t *testing.T) {
	t.Parallel()

	// EncodeParameter's signature has no request input: fragments are pure
	// values, so callers own URL assembly. Check the pieces can be assembled
	// explicitly without the encoder mutating anything.
	enc, err := EncodeParameter(mkParam("query", "p", "form", explode, stringArraySchema), []any{"a", "b"})
	require.NoError(t, err)
	parts := make([]string, len(enc.Query))
	for i, p := range enc.Query {
		parts[i] = p.Key + "=" + p.Value
	}
	require.Equal(t, "p=a&p=b", strings.Join(parts, "&"))
}
