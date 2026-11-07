// Package openapi3link evaluates OpenAPI 3 Link Objects at runtime.
//
// Eval resolves the Link's target operation inside a caller-provided set of
// already-loaded documents, then evaluates the Link's runtime expressions
// against the current request/response and the bodies the caller already
// decoded. It returns the target operation identity together with the named
// parameter and request body bindings.
//
// Eval never performs any network access, it never reads or closes an
// http.Request.Body/http.Response.Body, it does not implement parameter
// serialization, and it never injects schema defaults or mutates the
// documents, their schemas or the decoded bodies handed in by the caller.
package openapi3link

import (
	"fmt"
	"net/url"
	"strings"
)

// TargetLocation identifies a single operation declaration inside the
// provided document set.
type TargetLocation struct {
	// DocumentURI is the base URI of the document declaring the operation.
	DocumentURI *url.URL
	// Path is the URL path template under which the operation is declared,
	// for example "/users/{id}".
	Path string
	// Method is the HTTP method of the operation, for example "GET".
	Method string
}

func (l TargetLocation) String() string {
	doc := ""
	if l.DocumentURI != nil {
		doc = l.DocumentURI.String()
	}
	return doc + "#/paths" + l.Path + "/" + strings.ToLower(l.Method)
}

// BothTargetsError reports a Link that declares both operationId and
// operationRef. The Link Object allows at most one of them.
type BothTargetsError struct {
	OperationID  string
	OperationRef string
}

func (e *BothTargetsError) Error() string {
	return fmt.Sprintf("link declares both operationId %q and operationRef %q, expected exactly one",
		e.OperationID, e.OperationRef)
}

// NoTargetError reports a Link that declares neither operationId nor
// operationRef.
type NoTargetError struct{}

func (e *NoTargetError) Error() string {
	return "link declares neither operationId nor operationRef"
}

// DuplicateOperationIDError reports an operationId that is declared by more
// than one operation in the provided document set.
type DuplicateOperationIDError struct {
	OperationID string
	Locations   []TargetLocation
}

func (e *DuplicateOperationIDError) Error() string {
	where := make([]string, len(e.Locations))
	for i, l := range e.Locations {
		where[i] = l.String()
	}
	return fmt.Sprintf("operationId %q is not unique in the provided document set, declared at: %s",
		e.OperationID, strings.Join(where, ", "))
}

// OperationIDNotFoundError reports an operationId that no operation in the
// provided document set declares.
type OperationIDNotFoundError struct {
	OperationID string
}

func (e *OperationIDNotFoundError) Error() string {
	return fmt.Sprintf("operationId %q was not found in the provided document set", e.OperationID)
}

// InvalidOperationRefError reports an operationRef that is not a valid
// URI reference containing a JSON Pointer to an operation.
type InvalidOperationRefError struct {
	OperationRef string
	Reason       string
}

func (e *InvalidOperationRefError) Error() string {
	return fmt.Sprintf("invalid operationRef %q: %s", e.OperationRef, e.Reason)
}

// ExternalDocumentNotProvidedError reports an operationRef that points,
// outside its fragment, at a document that is not part of the provided
// document set. Eval never fetches external documents.
type ExternalDocumentNotProvidedError struct {
	OperationRef string
	// URI is the resolved document URI the operationRef needs.
	URI *url.URL
}

func (e *ExternalDocumentNotProvidedError) Error() string {
	return fmt.Sprintf("operationRef %q requires document %q, which is not part of the provided document set",
		e.OperationRef, e.URI.String())
}

// OperationRefTargetNotFoundError reports an operationRef whose JSON Pointer
// cannot be resolved in the referenced document, or that resolves to
// something that is not an operation reachable under a path template.
type OperationRefTargetNotFoundError struct {
	OperationRef string
	// DocumentURI is the document the pointer was evaluated against.
	DocumentURI *url.URL
	Reason      string
}

func (e *OperationRefTargetNotFoundError) Error() string {
	return fmt.Sprintf("operationRef %q does not resolve to an operation in %q: %s",
		e.OperationRef, e.DocumentURI.String(), e.Reason)
}

// AmbiguousTargetError reports an operationRef that resolves to an operation
// reachable under more than one path template, for example because a path
// item is reused under several paths. The target URL would not be unique.
type AmbiguousTargetError struct {
	OperationRef string
	Locations    []TargetLocation
}

func (e *AmbiguousTargetError) Error() string {
	where := make([]string, len(e.Locations))
	for i, l := range e.Locations {
		where[i] = l.String()
	}
	return fmt.Sprintf("operationRef %q resolves to an operation reachable under more than one path template: %s",
		e.OperationRef, strings.Join(where, ", "))
}
