package openapi3link_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3link"
	"github.com/stretchr/testify/require"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	require.NoError(t, err)
	return u
}

func newDoc() *openapi3.T {
	return &openapi3.T{
		OpenAPI: "3.0.3",
		Info:    &openapi3.Info{Title: "test", Version: "1"},
		Paths:   openapi3.NewPaths(),
	}
}

func newParam(in, name string, required bool, schema *openapi3.Schema) *openapi3.Parameter {
	p := &openapi3.Parameter{Name: name, In: in, Required: required}
	if schema != nil {
		p.Schema = &openapi3.SchemaRef{Value: schema}
	}
	return p
}

func refParam(p *openapi3.Parameter) *openapi3.ParameterRef { return &openapi3.ParameterRef{Value: p} }

func jsonRequestBody(required bool, schema *openapi3.Schema) *openapi3.RequestBodyRef {
	rb := openapi3.NewRequestBody().WithContent(openapi3.NewContentWithJSONSchema(schema))
	rb.Required = required
	return &openapi3.RequestBodyRef{Value: rb}
}

// diagAt returns the diagnostics at the given location, in result order.
func diagAt(res *openapi3link.Result, location string) []openapi3link.Diagnostic {
	var out []openapi3link.Diagnostic
	for _, d := range res.Diagnostics {
		if d.Location == location {
			out = append(out, d)
		}
	}
	return out
}

func codesAt(res *openapi3link.Result, location string) []openapi3link.DiagnosticCode {
	ds := diagAt(res, location)
	out := make([]openapi3link.DiagnosticCode, len(ds))
	for i, d := range ds {
		out[i] = d.Code
	}
	return out
}

func binding(res *openapi3link.Result, name string) (openapi3link.ParameterBinding, bool) {
	for _, b := range res.Parameters {
		if b.Name == name {
			return b, true
		}
	}
	return openapi3link.ParameterBinding{}, false
}

type countingReadCloser struct {
	data []byte
	pos  int
	read int
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	c.read++
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := copy(p, c.data[c.pos:])
	c.pos += n
	return n, nil
}

func (c *countingReadCloser) Close() error { return nil }

// TestEval_OperationRef_BaseDifferentFromRequest exercises the acceptance
// baseline: an escaped operationRef resolved against the document base while
// the request URL has a different base, all runtime expression sources, and a
// zero body-read count.
func TestEval_OperationRef_BaseDifferentFromRequest(t *testing.T) {
	doc := newDoc()
	doc.AddOperation("/users/{id}", http.MethodGet, &openapi3.Operation{
		OperationID: "getUser",
		Parameters: openapi3.Parameters{
			refParam(newParam(openapi3.ParameterInPath, "id", true, openapi3.NewStringSchema())),
			refParam(newParam(openapi3.ParameterInQuery, "verbose", false, nil)),
			refParam(newParam(openapi3.ParameterInHeader, "X-Trace", false, openapi3.NewStringSchema())),
			refParam(newParam(openapi3.ParameterInHeader, "X-Method", false, openapi3.NewStringSchema())),
			refParam(newParam(openapi3.ParameterInHeader, "X-Status", false, openapi3.NewIntegerSchema())),
			refParam(newParam(openapi3.ParameterInHeader, "X-Url", false, openapi3.NewStringSchema())),
		},
		RequestBody: jsonRequestBody(false, openapi3.NewObjectSchema()),
	})

	docURI := mustURL(t, "https://docs.example.com/specs/orders.yaml")
	reqURL := mustURL(t, "https://api.example.com/v2/orders/9?verbose=true")

	cr := &countingReadCloser{data: []byte(`{"raw":true}`)}
	httpReq := httptest.NewRequest(http.MethodPost, reqURL.String(), cr)
	httpReq.Header.Set("X-Trace", "abc")
	require.Equal(t, 0, cr.read)

	link := &openapi3.Link{
		OperationRef: "#/paths/~1users~1{id}/get",
		Parameters: map[string]any{
			"id":      "$response.body#/userId",
			"verbose": "$request.query.verbose",
			// header lookup is case-insensitive on both the expression and
			// the declared parameter name.
			"X-Trace":  "$request.header.X-TRACE",
			"X-Method": "$method",
			"X-Status": "$statusCode",
			"X-Url":    "$url",
		},
		RequestBody: "$response.body#/payload",
	}

	res, err := openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link:            link,
		Documents:       map[*url.URL]*openapi3.T{docURI: doc},
		LinkDocumentURI: docURI,
		Request: &openapi3link.Request{
			Method: httpReq.Method,
			URL:    httpReq.URL,
			Header: httpReq.Header,
			Query:  httpReq.URL.Query(),
			Body:   nil, // the caller decoded nothing; the link does not read it
		},
		Response: &openapi3link.Response{
			StatusCode: http.StatusCreated,
			Header:     http.Header{},
			Body: map[string]any{
				"userId":  42, // JSON number type preserved (not stringified)
				"payload": map[string]any{"name": "kin"},
			},
		},
	})
	require.NoError(t, err)

	// Identity.
	require.Equal(t, "https://docs.example.com/specs/orders.yaml", res.Target.DocumentURI.String())
	require.Equal(t, "/users/{id}", res.Target.Path)
	require.Equal(t, http.MethodGet, res.Target.Method)
	require.NotNil(t, res.Target.Operation)
	require.Equal(t, "getUser", res.Target.Operation.OperationID)

	// Bindings.
	id, ok := binding(res, "id")
	require.True(t, ok)
	require.Equal(t, "path", id.In)
	require.Equal(t, 42, id.Value, "body pointer preserves the JSON number type")

	verbose, ok := binding(res, "verbose")
	require.True(t, ok)
	require.Equal(t, "true", verbose.Value)

	trace, ok := binding(res, "X-Trace")
	require.True(t, ok)
	require.Equal(t, "abc", trace.Value)

	m, ok := binding(res, "X-Method")
	require.True(t, ok)
	require.Equal(t, http.MethodPost, m.Value)

	status, ok := binding(res, "X-Status")
	require.True(t, ok)
	require.Equal(t, http.StatusCreated, status.Value)

	u, ok := binding(res, "X-Url")
	require.True(t, ok)
	require.Equal(t, reqURL.String(), u.Value, "$url is the request URL, not the document base")

	// Request body binding.
	require.True(t, res.HasRequestBody)
	require.Equal(t, map[string]any{"name": "kin"}, res.RequestBody)

	// id=42 (number) vs string path parameter schema: mismatch reported but
	// the binding is retained.
	require.Equal(t, []openapi3link.DiagnosticCode{openapi3link.DiagSchemaMismatch}, codesAt(res, "parameters.id"))

	// No other problems.
	for _, d := range res.Diagnostics {
		require.Equal(t, openapi3link.DiagSchemaMismatch, d.Code, d)
	}

	// The original HTTP request body was never touched.
	require.Equal(t, 0, cr.read)
}

// TestEval_OperationRef_EscapedAndExternal covers pointer escaping and
// resolution into another provided document, plus the refusal to reach a
// document that was not provided.
func TestEval_OperationRef_EscapedAndExternal(t *testing.T) {
	other := newDoc()
	other.AddOperation("/items/{item}", http.MethodGet, &openapi3.Operation{
		Parameters: openapi3.Parameters{
			refParam(newParam(openapi3.ParameterInPath, "item", true, openapi3.NewStringSchema())),
		},
	})
	otherURI := mustURL(t, "https://docs.example.com/spec/b/other.yaml")

	main := newDoc()
	mainURI := mustURL(t, "https://docs.example.com/spec/main.yaml")

	link := &openapi3.Link{
		OperationRef: "b/other.yaml#/paths/~1items~1{item}/get",
		Parameters:   map[string]any{"item": "$response.body#/id"},
	}
	res, err := openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link:            link,
		Documents:       map[*url.URL]*openapi3.T{mainURI: main, otherURI: other},
		LinkDocumentURI: mainURI,
		Response:        &openapi3link.Response{StatusCode: 200, Body: map[string]any{"id": "x1"}},
	})
	require.NoError(t, err)
	require.Equal(t, otherURI.String(), res.Target.DocumentURI.String())
	require.Equal(t, "/items/{item}", res.Target.Path)
	require.Equal(t, http.MethodGet, res.Target.Method)
	item, ok := binding(res, "item")
	require.True(t, ok)
	require.Equal(t, "x1", item.Value)

	// Missing external document: no network access, typed error.
	linkMissing := &openapi3.Link{
		OperationRef: "b/missing.yaml#/paths/~1items/get",
	}
	var extErr *openapi3link.ExternalDocumentNotProvidedError
	_, err = openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link:            linkMissing,
		Documents:       map[*url.URL]*openapi3.T{mainURI: main, otherURI: other},
		LinkDocumentURI: mainURI,
	})
	require.ErrorAs(t, err, &extErr)
	require.Equal(t, "https://docs.example.com/spec/b/missing.yaml", extErr.URI.String())

	// Pure-fragment ref into an external host needs that document too.
	linkHost := &openapi3.Link{OperationRef: "https://elsewhere.test/api.yaml#/paths/~1x/get"}
	_, err = openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link:            linkHost,
		Documents:       map[*url.URL]*openapi3.T{mainURI: main},
		LinkDocumentURI: mainURI,
	})
	require.ErrorAs(t, err, &extErr)
}

// TestEval_OperationID_Errors covers duplicate and missing operationIds and
// the mutual-exclusion rules.
func TestEval_OperationID_Errors(t *testing.T) {
	docA := newDoc()
	docA.AddOperation("/a", http.MethodGet, &openapi3.Operation{OperationID: "dup"})
	docB := newDoc()
	docB.AddOperation("/b", http.MethodPost, &openapi3.Operation{OperationID: "dup"})
	docB.AddOperation("/c", http.MethodPut, &openapi3.Operation{OperationID: "uniq"})

	uA := mustURL(t, "https://docs.example.com/a.yaml")
	uB := mustURL(t, "https://docs.example.com/b.yaml")
	docs := map[*url.URL]*openapi3.T{uA: docA, uB: docB}

	_, err := openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link:            &openapi3.Link{OperationID: "dup"},
		Documents:       docs,
		LinkDocumentURI: uA,
	})
	var dupErr *openapi3link.DuplicateOperationIDError
	require.ErrorAs(t, err, &dupErr)
	require.Equal(t, "dup", dupErr.OperationID)
	require.Len(t, dupErr.Locations, 2)

	_, err = openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link:            &openapi3.Link{OperationID: "nope"},
		Documents:       docs,
		LinkDocumentURI: uA,
	})
	var nfErr *openapi3link.OperationIDNotFoundError
	require.ErrorAs(t, err, &nfErr)
	require.Equal(t, "nope", nfErr.OperationID)

	_, err = openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link:            &openapi3.Link{OperationID: "uniq", OperationRef: "#/x"},
		Documents:       docs,
		LinkDocumentURI: uA,
	})
	var bothErr *openapi3link.BothTargetsError
	require.ErrorAs(t, err, &bothErr)

	_, err = openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link:            &openapi3.Link{},
		Documents:       docs,
		LinkDocumentURI: uA,
	})
	var noErr *openapi3link.NoTargetError
	require.ErrorAs(t, err, &noErr)
}

// errorTarget returns an errors.As target for an error type whose Error
// method has a pointer receiver.
func errorTarget[T any]() any {
	var v *T
	return &v
}

// TestEval_OperationRef_TargetErrors covers unresolvable pointers,
// non-operation targets and path-reuse ambiguity.
func TestEval_OperationRef_TargetErrors(t *testing.T) {
	doc := newDoc()
	doc.AddOperation("/a/{id}", http.MethodGet, &openapi3.Operation{OperationID: "getA"})

	// The same path item is reused under two path templates: the target URL
	// of the operation would not be unique.
	shared := &openapi3.PathItem{Get: &openapi3.Operation{OperationID: "shared"}}
	doc.Paths.Set("/one/{id}", shared)
	doc.Paths.Set("/two/{id}", shared)

	docURI := mustURL(t, "https://docs.example.com/doc.yaml")

	cases := []struct {
		name string
		ref  string
		want any
	}{
		{"dangling pointer", "#/paths/~1missing/get", errorTarget[openapi3link.OperationRefTargetNotFoundError]()},
		{"points to non-operation", "#/paths/~1a~1{id}/get/operationId", errorTarget[openapi3link.OperationRefTargetNotFoundError]()},
		{"no fragment", "https://docs.example.com/doc.yaml", errorTarget[openapi3link.InvalidOperationRefError]()},
		{"fragment not a pointer", "#fragments-are-not-pointers", errorTarget[openapi3link.InvalidOperationRefError]()},
		{"path reuse ambiguity", "#/paths/~1one~1{id}/get", errorTarget[openapi3link.AmbiguousTargetError]()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := openapi3link.Eval(context.Background(), &openapi3link.Input{
				Link:            &openapi3.Link{OperationRef: tc.ref},
				Documents:       map[*url.URL]*openapi3.T{docURI: doc},
				LinkDocumentURI: docURI,
			})
			require.Error(t, err)
			require.True(t, errors.As(err, tc.want), "got %T: %v", err, err)
		})
	}

	// The ambiguity error lists both path templates.
	_, err := openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link:            &openapi3.Link{OperationRef: "#/paths/~1one~1{id}/get"},
		Documents:       map[*url.URL]*openapi3.T{docURI: doc},
		LinkDocumentURI: docURI,
	})
	var ambErr *openapi3link.AmbiguousTargetError
	require.ErrorAs(t, err, &ambErr)
	paths := []string{ambErr.Locations[0].Path, ambErr.Locations[1].Path}
	require.ElementsMatch(t, []string{"/one/{id}", "/two/{id}"}, paths)
}

// TestEval_PathItemParameterOverride verifies that an operation parameter
// overrides the same-name, same-location path-item parameter.
func TestEval_PathItemParameterOverride(t *testing.T) {
	doc := newDoc()
	op := &openapi3.Operation{
		OperationID: "op",
		Parameters: openapi3.Parameters{
			// Operation-level X-N is an integer; the path item declares string.
			refParam(newParam(openapi3.ParameterInHeader, "X-N", false, openapi3.NewIntegerSchema())),
		},
	}
	pi := &openapi3.PathItem{
		Get: op,
		Parameters: openapi3.Parameters{
			refParam(newParam(openapi3.ParameterInHeader, "X-N", false, openapi3.NewStringSchema())),
			refParam(newParam(openapi3.ParameterInQuery, "shared", false, openapi3.NewStringSchema())),
		},
	}
	doc.Paths.Set("/op", pi)
	docURI := mustURL(t, "https://docs.example.com/doc.yaml")

	res, err := openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link: &openapi3.Link{
			OperationID: "op",
			Parameters: map[string]any{
				"X-N":    7, // integer constant must pass thanks to the override
				"shared": "kept",
			},
		},
		Documents:       map[*url.URL]*openapi3.T{docURI: doc},
		LinkDocumentURI: docURI,
	})
	require.NoError(t, err)
	n, ok := binding(res, "X-N")
	require.True(t, ok)
	require.Equal(t, 7, n.Value)
	require.Empty(t, res.Diagnostics)
}

// TestEval_ParameterNameLocations covers qualification, header casing and
// ambiguity for a name declared in multiple locations.
func TestEval_ParameterNameLocations(t *testing.T) {
	doc := newDoc()
	doc.AddOperation("/x/{id}", http.MethodGet, &openapi3.Operation{
		Parameters: openapi3.Parameters{
			refParam(newParam(openapi3.ParameterInPath, "id", true, openapi3.NewStringSchema())),
			refParam(newParam(openapi3.ParameterInQuery, "id", false, openapi3.NewStringSchema())),
			refParam(newParam(openapi3.ParameterInHeader, "x-foo", false, openapi3.NewStringSchema())),
		},
	})
	docURI := mustURL(t, "https://docs.example.com/doc.yaml")

	// Unqualified name: ambiguous and required-missing.
	res, err := openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link: &openapi3.Link{
			OperationID:  "",
			OperationRef: "#/paths/~1x~1{id}/get",
			Parameters:   map[string]any{"id": "$request.path.id"},
		},
		Documents:       map[*url.URL]*openapi3.T{docURI: doc},
		LinkDocumentURI: docURI,
		Request: &openapi3link.Request{
			Method:     http.MethodGet,
			PathParams: map[string]string{"id": "p42"},
			Query:      url.Values{"id": {"q42"}},
		},
	})
	require.NoError(t, err)
	require.Equal(t, []openapi3link.DiagnosticCode{
		openapi3link.DiagAmbiguousParameterName,
		openapi3link.DiagRequiredParameterMissing,
	}, codesAt(res, "parameters.id"))
	_, ok := binding(res, "id")
	require.False(t, ok)

	// Qualified names disambiguate; path/query names are case-sensitive.
	res, err = openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link: &openapi3.Link{
			OperationRef: "#/paths/~1x~1{id}/get",
			Parameters: map[string]any{
				"path.id":  "$request.path.id",
				"query.id": "$request.query.id",
				// Header qualifier and declared name differ in case.
				"header.X-FOO": "$request.header.x-foo",
			},
		},
		Documents:       map[*url.URL]*openapi3.T{docURI: doc},
		LinkDocumentURI: docURI,
		Request: &openapi3link.Request{
			Method:     http.MethodGet,
			PathParams: map[string]string{"id": "p42"},
			Query:      url.Values{"id": {"q42"}},
			Header:     http.Header{"X-Foo": {"v"}},
		},
	})
	require.NoError(t, err)
	require.Empty(t, res.Diagnostics)
	p, ok := binding(res, "id") // first "id" binding; both locations are present
	require.True(t, ok)
	require.Equal(t, "p42", p.Value)
	var inValues []string
	for _, b := range res.Parameters {
		if b.Name == "id" {
			inValues = append(inValues, b.In)
		}
	}
	require.ElementsMatch(t, []string{"path", "query"}, inValues)
	h, ok := binding(res, "x-foo")
	require.True(t, ok)
	require.Equal(t, "v", h.Value)
}

// TestEval_BodyPointerTypes covers zero/false/null vs missing pointers,
// array indices, empty properties and pointer escaping.
func TestEval_BodyPointerTypes(t *testing.T) {
	doc := newDoc()
	doc.AddOperation("/p", http.MethodPost, &openapi3.Operation{
		Parameters: openapi3.Parameters{
			refParam(newParam(openapi3.ParameterInQuery, "zero", false, openapi3.NewIntegerSchema())),
			refParam(newParam(openapi3.ParameterInQuery, "no", false, openapi3.NewBoolSchema())),
			refParam(newParam(openapi3.ParameterInQuery, "nul", false, nil)), // typeless: null allowed
			refParam(newParam(openapi3.ParameterInQuery, "first", false, openapi3.NewIntegerSchema())),
			refParam(newParam(openapi3.ParameterInQuery, "slash", false, openapi3.NewStringSchema())),
			refParam(newParam(openapi3.ParameterInQuery, "tilde", false, openapi3.NewStringSchema())),
			refParam(newParam(openapi3.ParameterInQuery, "empty", false, openapi3.NewStringSchema())),
			refParam(newParam(openapi3.ParameterInQuery, "miss", true, openapi3.NewIntegerSchema())),
			refParam(newParam(openapi3.ParameterInQuery, "dash", false, openapi3.NewIntegerSchema())),
			refParam(newParam(openapi3.ParameterInQuery, "badidx", false, openapi3.NewIntegerSchema())),
			refParam(newParam(openapi3.ParameterInQuery, "deepmiss", false, nil)),
		},
	})
	docURI := mustURL(t, "https://docs.example.com/doc.yaml")

	body := map[string]any{
		"zero": 0,
		"no":   false,
		"nul":  nil,
		"arr": []any{
			10,
			map[string]any{
				"a/b": "slash-value", // ~1
				"a~b": "tilde-value", // ~0
				"":    "empty-value",
			},
		},
		"obj": map[string]any{"exist": "v"},
	}

	link := &openapi3.Link{
		OperationRef: "#/paths/~1p/post",
		Parameters: map[string]any{
			"zero":     "$response.body#/zero",
			"no":       "$response.body#/no",
			"nul":      "$response.body#/nul",
			"first":    "$response.body#/arr/0",
			"slash":    "$response.body#/arr/1/a~1b",
			"tilde":    "$response.body#/arr/1/a~0b",
			"empty":    "$response.body#/arr/1/",
			"miss":     "$response.body#/obj/missing", // absent property
			"dash":     "$response.body#/arr/-",       // nonelement
			"badidx":   "$response.body#/arr/01",      // leading zero
			"deepmiss": "$response.body#/obj/missing/again",
		},
	}
	res, err := openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link:            link,
		Documents:       map[*url.URL]*openapi3.T{docURI: doc},
		LinkDocumentURI: docURI,
		Response:        &openapi3link.Response{StatusCode: 200, Body: body},
	})
	require.NoError(t, err)

	// Zero, false and explicit null are present with their native values.
	b, ok := binding(res, "zero")
	require.True(t, ok)
	require.Equal(t, 0, b.Value)
	b, ok = binding(res, "no")
	require.True(t, ok)
	require.Equal(t, false, b.Value)
	b, ok = binding(res, "nul")
	require.True(t, ok)
	require.Nil(t, b.Value, "JSON null binds as nil but the binding is present")

	b, ok = binding(res, "first")
	require.True(t, ok)
	require.Equal(t, 10, b.Value)
	b, _ = binding(res, "slash")
	require.Equal(t, "slash-value", b.Value)
	b, _ = binding(res, "tilde")
	require.Equal(t, "tilde-value", b.Value)
	b, _ = binding(res, "empty")
	require.Equal(t, "empty-value", b.Value)

	// Missing / malformed references omit the binding.
	for _, name := range []string{"miss", "dash", "badidx", "deepmiss"} {
		_, ok := binding(res, name)
		require.False(t, ok, name)
	}

	require.Equal(t, []openapi3link.DiagnosticCode{
		openapi3link.DiagExpressionNoValue,
		openapi3link.DiagRequiredParameterMissing,
	}, codesAt(res, "parameters.miss"))
	require.Equal(t, []openapi3link.DiagnosticCode{openapi3link.DiagExpressionNoValue}, codesAt(res, "parameters.dash"))
	require.Equal(t, []openapi3link.DiagnosticCode{openapi3link.DiagInvalidExpression}, codesAt(res, "parameters.badidx"))
	require.Equal(t, []openapi3link.DiagnosticCode{openapi3link.DiagExpressionNoValue}, codesAt(res, "parameters.deepmiss"))

	// The input body was not modified.
	require.Equal(t, 0, body["zero"])
	require.Equal(t, false, body["no"])
	require.Contains(t, body, "nul")
	require.Nil(t, body["nul"])
}

// TestEval_MultiValues verifies lossless array binding and ambiguity errors
// for repeated headers/query parameters.
func TestEval_MultiValues(t *testing.T) {
	doc := newDoc()
	doc.AddOperation("/p", http.MethodGet, &openapi3.Operation{
		Parameters: openapi3.Parameters{
			refParam(newParam(openapi3.ParameterInHeader, "marr", false,
				openapi3.NewArraySchema().WithItems(openapi3.NewStringSchema()))),
			refParam(newParam(openapi3.ParameterInHeader, "mscalar", true, openapi3.NewStringSchema())),
			refParam(newParam(openapi3.ParameterInHeader, "one", false, openapi3.NewStringSchema())),
			refParam(newParam(openapi3.ParameterInQuery, "qarr", false,
				openapi3.NewArraySchema().WithItems(openapi3.NewStringSchema()))),
			refParam(newParam(openapi3.ParameterInQuery, "qscalar", false, openapi3.NewStringSchema())),
		},
	})
	docURI := mustURL(t, "https://docs.example.com/doc.yaml")

	res, err := openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link: &openapi3.Link{
			OperationRef: "#/paths/~1p/get",
			Parameters: map[string]any{
				"marr":    "$request.header.x-multi",
				"mscalar": "$request.header.x-multi",
				"one":     "$request.header.x-one",
				"qarr":    "$request.query.q",
				"qscalar": "$request.query.q",
			},
		},
		Documents:       map[*url.URL]*openapi3.T{docURI: doc},
		LinkDocumentURI: docURI,
		Request: &openapi3link.Request{
			Method: http.MethodGet,
			Header: http.Header{
				"X-Multi": {"a", "b"},
				"X-One":   {"only"},
			},
			Query: url.Values{"q": {"1", "2"}},
		},
	})
	require.NoError(t, err)

	marr, ok := binding(res, "marr")
	require.True(t, ok)
	require.Equal(t, []any{"a", "b"}, marr.Value)
	qarr, ok := binding(res, "qarr")
	require.True(t, ok)
	require.Equal(t, []any{"1", "2"}, qarr.Value)
	one, ok := binding(res, "one")
	require.True(t, ok)
	require.Equal(t, "only", one.Value)

	require.Equal(t, []openapi3link.DiagnosticCode{
		openapi3link.DiagAmbiguousMultiValue,
		openapi3link.DiagRequiredParameterMissing,
	}, codesAt(res, "parameters.mscalar"))
	require.Equal(t, []openapi3link.DiagnosticCode{openapi3link.DiagAmbiguousMultiValue},
		codesAt(res, "parameters.qscalar"))
}

// TestEval_ConstantsAndInvalidExpressions covers constant vs expression
// discrimination, invalid expressions, unknown target parameters and schema
// mismatches.
func TestEval_ConstantsAndInvalidExpressions(t *testing.T) {
	doc := newDoc()
	doc.AddOperation("/p", http.MethodPost, &openapi3.Operation{
		Parameters: openapi3.Parameters{
			refParam(newParam(openapi3.ParameterInQuery, "n", false, openapi3.NewIntegerSchema())),
			refParam(newParam(openapi3.ParameterInQuery, "s", false, openapi3.NewStringSchema())),
			refParam(newParam(openapi3.ParameterInQuery, "bad", true, openapi3.NewStringSchema())),
			refParam(newParam(openapi3.ParameterInQuery, "unknown", false, nil)),
		},
	})
	docURI := mustURL(t, "https://docs.example.com/doc.yaml")

	res, err := openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link: &openapi3.Link{
			OperationRef: "#/paths/~1p/post",
			Parameters: map[string]any{
				"n":            123,          // numeric constant
				"s":            "dollar",     // plain string constant
				"bad":          "$nope",      // starts with $ but is not an expression
				"unknown":      "$method",    // target parameter present, keep
				"ghost":        "constant",   // target has no such parameter
				"notargetexpr": "$request.x", // unknown target, expression is not evaluated
			},
		},
		Documents:       map[*url.URL]*openapi3.T{docURI: doc},
		LinkDocumentURI: docURI,
		Request:         &openapi3link.Request{Method: http.MethodPost},
	})
	require.NoError(t, err)

	n, ok := binding(res, "n")
	require.True(t, ok)
	require.Equal(t, 123, n.Value)
	s, ok := binding(res, "s")
	require.True(t, ok)
	require.Equal(t, "dollar", s.Value)
	u, ok := binding(res, "unknown")
	require.True(t, ok)
	require.Equal(t, http.MethodPost, u.Value)

	require.Equal(t, []openapi3link.DiagnosticCode{
		openapi3link.DiagInvalidExpression,
		openapi3link.DiagRequiredParameterMissing,
	}, codesAt(res, "parameters.bad"))
	require.Equal(t, []openapi3link.DiagnosticCode{openapi3link.DiagUnknownTargetParameter},
		codesAt(res, "parameters.ghost"))
	require.Equal(t, []openapi3link.DiagnosticCode{openapi3link.DiagUnknownTargetParameter},
		codesAt(res, "parameters.notargetexpr"))
}

// TestEval_RequestBody covers body binding validation and required handling.
func TestEval_RequestBody(t *testing.T) {
	doc := newDoc()
	doc.AddOperation("/p", http.MethodPost, &openapi3.Operation{
		RequestBody: jsonRequestBody(true, openapi3.NewObjectSchema().WithRequired([]string{"name"})),
	})
	docURI := mustURL(t, "https://docs.example.com/doc.yaml")

	// Whole response body bound as the request body.
	res, err := openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link: &openapi3.Link{
			OperationRef: "#/paths/~1p/post",
			RequestBody:  "$response.body",
		},
		Documents:       map[*url.URL]*openapi3.T{docURI: doc},
		LinkDocumentURI: docURI,
		Response: &openapi3link.Response{
			StatusCode: 200,
			Body:       map[string]any{"name": "kin"},
		},
	})
	require.NoError(t, err)
	require.True(t, res.HasRequestBody)
	require.Equal(t, map[string]any{"name": "kin"}, res.RequestBody)
	require.Empty(t, res.Diagnostics)

	// Missing pointer on a required body: omitted + required missing.
	res, err = openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link: &openapi3.Link{
			OperationRef: "#/paths/~1p/post",
			RequestBody:  "$response.body#/nope",
		},
		Documents:       map[*url.URL]*openapi3.T{docURI: doc},
		LinkDocumentURI: docURI,
		Response:        &openapi3link.Response{StatusCode: 200, Body: map[string]any{}},
	})
	require.NoError(t, err)
	require.False(t, res.HasRequestBody)
	require.Equal(t, []openapi3link.DiagnosticCode{
		openapi3link.DiagExpressionNoValue,
		openapi3link.DiagRequiredParameterMissing,
	}, codesAt(res, "requestBody"))
}

// TestEval_DefaultsNotInjectedAndBodyUnmodified verifies that schema defaults
// are never written into produced values or the caller's decoded body.
func TestEval_DefaultsNotInjectedAndBodyUnmodified(t *testing.T) {
	schema := openapi3.NewObjectSchema()
	prop := openapi3.NewStringSchema()
	prop.Default = "B"
	schema.Properties = map[string]*openapi3.SchemaRef{"a": {Value: openapi3.NewIntegerSchema()}, "b": {Value: prop}}

	doc := newDoc()
	doc.AddOperation("/p", http.MethodPost, &openapi3.Operation{
		RequestBody: jsonRequestBody(false, schema),
	})
	docURI := mustURL(t, "https://docs.example.com/doc.yaml")

	inputBody := map[string]any{"a": 1}
	res, err := openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link: &openapi3.Link{
			OperationRef: "#/paths/~1p/post",
			RequestBody:  "$response.body",
		},
		Documents:       map[*url.URL]*openapi3.T{docURI: doc},
		LinkDocumentURI: docURI,
		Response:        &openapi3link.Response{StatusCode: 200, Body: inputBody},
	})
	require.NoError(t, err)
	require.Equal(t, map[string]any{"a": 1}, res.RequestBody)
	require.Len(t, inputBody, 1, "no default injected into the caller body")
	_, present := inputBody["b"]
	require.False(t, present)
	// Shared schema keeps its default.
	require.Equal(t, "B", schema.Properties["b"].Value.Default)
}

// TestEval_OpenAPI31NullableType exercises the 3.1 type-array schema path.
func TestEval_OpenAPI31NullableType(t *testing.T) {
	doc := &openapi3.T{
		OpenAPI: "3.1.0",
		Info:    &openapi3.Info{Title: "t", Version: "1"},
		Paths:   openapi3.NewPaths(),
	}
	nullable := &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString, openapi3.TypeNull}}
	doc.AddOperation("/p", http.MethodGet, &openapi3.Operation{
		Parameters: openapi3.Parameters{
			refParam(newParam(openapi3.ParameterInQuery, "v", false, nullable)),
		},
	})
	docURI := mustURL(t, "https://docs.example.com/doc.yaml")

	res, err := openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link: &openapi3.Link{
			OperationRef: "#/paths/~1p/get",
			Parameters:   map[string]any{"v": "$response.body#/v"},
		},
		Documents:       map[*url.URL]*openapi3.T{docURI: doc},
		LinkDocumentURI: docURI,
		Response:        &openapi3link.Response{StatusCode: 200, Body: map[string]any{"v": nil}},
	})
	require.NoError(t, err)
	v, ok := binding(res, "v")
	require.True(t, ok)
	require.Nil(t, v.Value)
	require.Empty(t, res.Diagnostics)
}

// TestEval_SourceUnavailable verifies that referencing an unsupplied side or
// body produces the source-unavailable diagnostic rather than an empty value.
func TestEval_SourceUnavailable(t *testing.T) {
	doc := newDoc()
	doc.AddOperation("/p", http.MethodGet, &openapi3.Operation{
		Parameters: openapi3.Parameters{
			refParam(newParam(openapi3.ParameterInHeader, "h", false, openapi3.NewStringSchema())),
			refParam(newParam(openapi3.ParameterInQuery, "b", false, nil)),
		},
	})
	docURI := mustURL(t, "https://docs.example.com/doc.yaml")

	res, err := openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link: &openapi3.Link{
			OperationRef: "#/paths/~1p/get",
			Parameters: map[string]any{
				"h": "$response.header.x-missing",
				"b": "$response.body#/x",
			},
		},
		Documents:       map[*url.URL]*openapi3.T{docURI: doc},
		LinkDocumentURI: docURI,
		// Response deliberately omitted.
	})
	require.NoError(t, err)
	require.Equal(t, []openapi3link.DiagnosticCode{openapi3link.DiagSourceUnavailable}, codesAt(res, "parameters.h"))
	require.Equal(t, []openapi3link.DiagnosticCode{openapi3link.DiagSourceUnavailable}, codesAt(res, "parameters.b"))
}

// TestEval_LoadedDocument is an end-to-end test over a loader-loaded document
// with $ref-resolved parameters and a Link nested in a response.
func TestEval_LoadedDocument(t *testing.T) {
	spec := []byte(`
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /users/{id}:
    parameters:
      - name: id
        in: path
        required: true
        schema: {type: string}
    get:
      operationId: getUser
      parameters:
        - $ref: '#/components/parameters/Trace'
      responses:
        '200': {description: ok}
  /orders:
    post:
      operationId: createOrder
      responses:
        '201':
          description: created
          links:
            user:
              operationRef: '#/paths/~1users~1{id}/get'
              parameters:
                id: '$response.body#/userId'
                X-Trace: '$request.header.x-trace'
components:
  parameters:
    Trace:
      name: X-Trace
      in: header
      schema: {type: string}
`)
	docURI := mustURL(t, "https://docs.example.com/loaded.yaml")
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromDataWithPath(spec, docURI)
	require.NoError(t, err)
	require.NoError(t, loader.ResolveRefsIn(doc, docURI))

	link := doc.Paths.Value("/orders").Post.Responses.Value("201").Value.Links["user"].Value
	require.NotNil(t, link)

	res, err := openapi3link.Eval(context.Background(), &openapi3link.Input{
		Link:            link,
		Documents:       map[*url.URL]*openapi3.T{docURI: doc},
		LinkDocumentURI: docURI,
		Request: &openapi3link.Request{
			Method: http.MethodPost,
			URL:    mustURL(t, "https://api.example.com/orders"),
			Header: http.Header{"X-Trace": {"trace-1"}},
		},
		Response: &openapi3link.Response{
			StatusCode: 201,
			Body:       map[string]any{"userId": "u-9"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, "/users/{id}", res.Target.Path)
	require.Equal(t, http.MethodGet, res.Target.Method)
	require.Equal(t, "getUser", res.Target.Operation.OperationID)

	id, ok := binding(res, "id")
	require.True(t, ok)
	require.Equal(t, "u-9", id.Value)
	tr, ok := binding(res, "X-Trace")
	require.True(t, ok)
	require.Equal(t, "trace-1", tr.Value)
	require.Empty(t, res.Diagnostics)
}

// TestLinkValidateUnchanged confirms the definition-level Validate behavior
// is untouched.
func TestLinkValidateUnchanged(t *testing.T) {
	require.Error(t, (&openapi3.Link{}).Validate(context.Background()))
	require.Error(t, (&openapi3.Link{OperationID: "a", OperationRef: "#/b"}).Validate(context.Background()))
	require.NoError(t, (&openapi3.Link{OperationID: "a"}).Validate(context.Background()))
}
