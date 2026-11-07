package openapi3filter

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/routers"
)

// TestNegotiateResponse_ValidateResponse feeds the concrete content type
// chosen by Content.NegotiateResponse into ValidateResponse and checks the
// body against the negotiated MediaType's schema. It proves that the schema
// negotiation selects is the schema ValidateResponse actually enforces for
// the concrete Content-Type produced.
func TestNegotiateResponse_ValidateResponse(t *testing.T) {
	t.Parallel()

	// application/json carries an object that must contain "a";
	// the application/* range carries an object that must contain "b".
	schemaA := openapi3.NewObjectSchema()
	schemaA.Required = []string{"a"}
	schemaA.Properties = map[string]*openapi3.SchemaRef{"a": {Value: openapi3.NewIntegerSchema()}}
	schemaB := openapi3.NewObjectSchema()
	schemaB.Required = []string{"b"}
	schemaB.Properties = map[string]*openapi3.SchemaRef{"b": {Value: openapi3.NewIntegerSchema()}}

	content := openapi3.Content{
		"application/json": openapi3.NewMediaType().WithSchema(schemaA),
		"application/*":    openapi3.NewMediaType().WithSchema(schemaB),
	}

	doc := &openapi3.T{OpenAPI: "3.0.3"}
	op := &openapi3.Operation{
		Responses: openapi3.NewResponses(openapi3.WithStatus(http.StatusOK, &openapi3.ResponseRef{
			Value: openapi3.NewResponse().WithDescription("ok").WithContent(content),
		})),
	}
	route := &routers.Route{
		Spec:      doc,
		Operation: op,
		Method:    http.MethodGet,
		Path:      "/things",
		PathItem:  &openapi3.PathItem{Get: op},
	}

	// The server can concretely produce these two, in this preference order.
	offers := []string{"application/json", "application/yaml"}

	validate := func(t *testing.T, contentType, body string) error {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/things", nil)
		return ValidateResponse(t.Context(), &ResponseValidationInput{
			RequestValidationInput: &RequestValidationInput{Request: req, Route: route},
			Status:                 http.StatusOK,
			Header:                 http.Header{headerCT: {contentType}},
			Body:                   io.NopCloser(strings.NewReader(body)),
		})
	}

	t.Run("accept prefers yaml concretized from type wildcard", func(t *testing.T) {
		choice, err := content.NegotiateResponse(offers, "application/yaml;q=0.9, application/json;q=0.8")
		require.NoError(t, err)
		require.Equal(t, "application/yaml", choice.ContentType)
		require.Equal(t, "application/*", choice.ContentKey)
		require.Same(t, content["application/*"], choice.MediaType)
		require.Equal(t, "application/yaml", choice.AcceptEntry.MediaRange())
		// ValidateResponse resolves the concrete Content-Type to the same entry.
		require.Same(t, choice.MediaType, content.Get(choice.ContentType))

		require.NoError(t, validate(t, choice.ContentType, "b: 1\n"))
		require.ErrorContains(t, validate(t, choice.ContentType, "a: 1\n"),
			"response body doesn't match schema")
	})

	t.Run("exact q=0 excludes json even though */* is accepted", func(t *testing.T) {
		choice, err := content.NegotiateResponse(offers, "application/json;q=0, */*;q=1")
		require.NoError(t, err)
		require.Equal(t, "application/yaml", choice.ContentType)
		require.Equal(t, "*/*", choice.AcceptEntry.MediaRange())
		require.Same(t, content["application/*"], choice.MediaType)

		require.NoError(t, validate(t, choice.ContentType, "b: 1\n"))
		require.ErrorContains(t, validate(t, choice.ContentType, "a: 1\n"),
			"response body doesn't match schema")
	})

	t.Run("missing Accept keeps the first server offer and its schema", func(t *testing.T) {
		choice, err := content.NegotiateResponse(offers)
		require.NoError(t, err)
		require.Equal(t, "application/json", choice.ContentType)
		require.Nil(t, choice.AcceptEntry)
		require.Same(t, content["application/json"], choice.MediaType)

		require.NoError(t, validate(t, choice.ContentType, `{"a":1}`))
		require.ErrorContains(t, validate(t, choice.ContentType, `{"b":1}`),
			"response body doesn't match schema")
	})

	t.Run("not acceptable and undocumented offers surface errors", func(t *testing.T) {
		_, err := content.NegotiateResponse(offers, "text/html")
		require.ErrorIs(t, err, openapi3.ErrNotAcceptable)

		_, err = content.NegotiateResponse([]string{"image/png"}, "*/*")
		var uncovered *openapi3.OfferNotDocumentedError
		require.ErrorAs(t, err, &uncovered)

		_, err = content.NegotiateResponse(offers, "application/json;q=2")
		var badAccept *openapi3.AcceptParseError
		require.ErrorAs(t, err, &badAccept)
	})
}
