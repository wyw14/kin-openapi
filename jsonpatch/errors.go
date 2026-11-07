package jsonpatch

import (
	"errors"
	"fmt"
	"strings"
)

// Kind classifies an error returned while applying or validating a JSON Patch.
type Kind int

const (
	// KindSyntax is a malformed patch document or operation: unknown or missing
	// members, an invalid JSON Pointer, bad array indices, a move into a
	// descendant of its source, or a breached limit.
	KindSyntax Kind = iota + 1
	// KindTargetNotFound means a JSON Pointer does not resolve to an existing
	// value (remove/replace/test of something missing, "-" used outside of add,
	// traversal through a scalar, operating on a document whose root was removed).
	KindTargetNotFound
	// KindTestFailure means a test operation found its target but its value did
	// not compare equal to the expected JSON value.
	KindTestFailure
	// KindSchema means all operations applied but the final resource did not
	// validate against the target schema, or the patch removed the root so that
	// there is no value left to validate.
	KindSchema
)

// Error describes why a patch application failed. Every concrete error below
// implements Error.
type Error interface {
	error
	// Kind returns the error category.
	Kind() Kind
	// OperationIndex returns the 0-based index of the failing operation, or -1
	// when the error is not tied to a single operation.
	OperationIndex() int
	// Pointer returns the raw RFC 6901 JSON Pointer involved ("" for the root).
	Pointer() string
	// Field returns the operation member involved ("op", "path", "from",
	// "value") or "" when no particular member is involved.
	Field() string
}

// opError is the common shape of operation-level failures.
type opError struct {
	opIndex int
	field   string // "op", "path", "from", "value" or ""
	pointer string // raw JSON Pointer of path/from when known
	reason  string
	cause   error
}

func (e opError) OperationIndex() int { return e.opIndex }
func (e opError) Pointer() string     { return e.pointer }

// Field returns the operation member the error concerns: "op", "path",
// "from", "value", or "" when no particular member is involved.
func (e opError) Field() string { return e.field }

func (e opError) Error() string {
	var b strings.Builder
	b.WriteString(e.reason)
	if e.field != "" {
		fmt.Fprintf(&b, " (member %q", e.field)
		if e.pointer != "" {
			fmt.Fprintf(&b, ": %q", e.pointer)
		}
		b.WriteString(")")
	} else if e.pointer != "" {
		fmt.Fprintf(&b, " (pointer %q)", e.pointer)
	}
	if e.opIndex >= 0 {
		fmt.Fprintf(&b, " [operation %d]", e.opIndex)
	}
	if e.cause != nil {
		fmt.Fprintf(&b, ": %v", e.cause)
	}
	return b.String()
}

func (e opError) Unwrap() error { return e.cause }

// SyntaxError is a malformed patch document or operation.
type SyntaxError struct{ opError }

// Kind returns KindSyntax.
func (e *SyntaxError) Kind() Kind { return KindSyntax }

// TargetNotFoundError reports a JSON Pointer that does not resolve to a value.
type TargetNotFoundError struct{ opError }

// Kind returns KindTargetNotFound.
func (e *TargetNotFoundError) Kind() Kind { return KindTargetNotFound }

// TestFailureError reports a failed value comparison in a test operation.
type TestFailureError struct {
	opError
	// Expected is the value carried by the test operation.
	Expected any
	// Actual is the value found at the test pointer.
	Actual any
}

// Kind returns KindTestFailure.
func (e *TestFailureError) Kind() Kind { return KindTestFailure }

// SchemaValidationError reports that the patched resource does not validate
// against the target schema. It wraps the openapi3 validation error.
type SchemaValidationError struct {
	// Pointer is the RFC 6901 pointer to the invalid value in the final
	// resource, "" being the root.
	pointer string
	reason  string
	cause   error
}

// Kind returns KindSchema.
func (e *SchemaValidationError) Kind() Kind { return KindSchema }

// OperationIndex always returns -1: the resource is only validated once every
// operation has applied.
func (e *SchemaValidationError) OperationIndex() int { return -1 }

// Pointer returns the JSON Pointer to the value the schema rejected.
func (e *SchemaValidationError) Pointer() string { return e.pointer }

// Field returns "" because the final resource is validated as a whole.
func (e *SchemaValidationError) Field() string { return "" }

// Cause returns the wrapped openapi3 schema error, if any.
func (e *SchemaValidationError) Cause() error { return e.cause }

func (e *SchemaValidationError) Error() string {
	var b strings.Builder
	if e.reason != "" {
		b.WriteString(e.reason)
	} else {
		b.WriteString("patched resource does not validate against the target schema")
	}
	if e.pointer != "" {
		fmt.Fprintf(&b, " at pointer %q", e.pointer)
	}
	if e.cause != nil {
		fmt.Fprintf(&b, ": %v", e.cause)
	}
	return b.String()
}

func (e *SchemaValidationError) Unwrap() error { return e.cause }

// ErrUnresolvedTargetSchema is returned when the target SchemaRef carries no
// resolved Schema. This version does not load (external) references.
var ErrUnresolvedTargetSchema = errors.New("jsonpatch: target schema is not resolved")

func syntaxErrorf(opIndex int, field, pointer, format string, args ...any) *SyntaxError {
	return &SyntaxError{opError: opError{
		opIndex: opIndex,
		field:   field,
		pointer: pointer,
		reason:  fmt.Sprintf(format, args...),
	}}
}

func targetNotFoundErrorf(opIndex int, field, pointer, format string, args ...any) *TargetNotFoundError {
	return &TargetNotFoundError{opError: opError{
		opIndex: opIndex,
		field:   field,
		pointer: pointer,
		reason:  fmt.Sprintf(format, args...),
	}}
}
