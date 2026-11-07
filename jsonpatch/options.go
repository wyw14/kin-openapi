package jsonpatch

import "github.com/getkin/kin-openapi/openapi3"

const (
	// DefaultMaxOperations bounds the number of operations in a single patch.
	DefaultMaxOperations = 1000
	// DefaultMaxPointerDepth bounds the number of reference tokens in a JSON Pointer.
	DefaultMaxPointerDepth = 1000
)

type options struct {
	maxOperations           int
	maxPointerDepth         int
	schemaValidationOptions []openapi3.SchemaValidationOption
}

// Option customizes [Apply] and [ApplyJSON].
type Option func(*options)

// WithMaxOperations limits the number of operations in the patch. Values less
// than 1 are ignored.
func WithMaxOperations(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxOperations = n
		}
	}
}

// WithMaxPointerDepth limits how many reference tokens a single "path" or
// "from" JSON Pointer may contain. Values less than 1 are ignored.
func WithMaxPointerDepth(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxPointerDepth = n
		}
	}
}

// WithSchemaValidationOptions appends options used when the final resource is
// visited against the target schema. Validation always runs in the request
// direction ([openapi3.VisitAsRequest]) and never injects defaults; pass for
// example [openapi3.EnableJSONSchema2020] for OpenAPI 3.1 documents.
func WithSchemaValidationOptions(opts ...openapi3.SchemaValidationOption) Option {
	return func(o *options) {
		o.schemaValidationOptions = append(o.schemaValidationOptions, opts...)
	}
}

func evalOptions(opts ...Option) *options {
	o := &options{
		maxOperations:   DefaultMaxOperations,
		maxPointerDepth: DefaultMaxPointerDepth,
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}
