package openapi3filter

import "github.com/getkin/kin-openapi/openapi3"

// Options used by ValidateRequest and ValidateResponse
type Options struct {
	// Set ExcludeRequestBody so ValidateRequest skips request body validation
	ExcludeRequestBody bool

	// Set ExcludeRequestQueryParams so ValidateRequest skips request query params validation
	ExcludeRequestQueryParams bool

	// Set ExcludeResponseBody so ValidateResponse skips response body validation
	ExcludeResponseBody bool

	// Set ExcludeReadOnlyValidations so ValidateRequest skips read-only validations
	ExcludeReadOnlyValidations bool

	// Set ExcludeWriteOnlyValidations so ValidateResponse skips write-only validations
	ExcludeWriteOnlyValidations bool

	// Set IncludeResponseStatus so ValidateResponse fails on response
	// status not defined in OpenAPI spec
	IncludeResponseStatus bool

	MultiError bool

	// Set RegexCompiler to override the regex implementation
	RegexCompiler openapi3.RegexCompilerFunc

	// Set RejectWhenRequestBodyNotSpecified so ValidateRequest fails when request body is present but not defined in the specification
	RejectWhenRequestBodyNotSpecified bool

	// A document with security schemes defined will not pass validation
	// unless an AuthenticationFunc is defined.
	// See NoopAuthenticationFunc
	AuthenticationFunc AuthenticationFunc

	// Indicates whether default values are set in the
	// request. If true, then they are not set
	SkipSettingDefaults bool

	// Set KeepValidatedValue so ValidateRequest gives req.Body a ValidatedValue() any
	// method returning the validated request body value, defaults included
	KeepValidatedValue bool

	// GzipRequestBody, when non-nil, enables explicit validation of JSON
	// ("application/json" or a "+json" media type) request bodies sent with
	// "Content-Encoding: gzip": the body is decompressed and the decoded value
	// is validated against the requestBody schema. The request itself is left
	// compressed for downstream handlers.
	//
	// When nil (the default), request bodies are read as-is and behavior is
	// unchanged. Only identity or a single gzip content coding is accepted;
	// content-coding stacks and any other coding are rejected before the body
	// is read. See GzipRequestBodyOptions for details.
	GzipRequestBody *GzipRequestBodyOptions

	customSchemaErrorFunc CustomSchemaErrorFunc

	// Additional schema validation options to pass through to schema validation.
	// Use this to pass document-scoped format validators or other per-validation options.
	SchemaValidationOptions []openapi3.SchemaValidationOption
}

// CustomSchemaErrorFunc allows for custom the schema error message.
type CustomSchemaErrorFunc func(err *openapi3.SchemaError) string

// WithCustomSchemaErrorFunc sets a function to override the schema error message.
// If the passed function returns an empty string, it returns to the previous Error() implementation.
func (o *Options) WithCustomSchemaErrorFunc(f CustomSchemaErrorFunc) {
	o.customSchemaErrorFunc = f
}
