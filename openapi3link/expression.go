package openapi3link

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// DiagnosticCode classifies a non-fatal finding reported while evaluating a
// Link. Target resolution failures are returned as Go errors instead.
type DiagnosticCode string

const (
	// DiagInvalidExpression reports a runtime expression that does not comply
	// with the OpenAPI runtime expression grammar, or whose JSON Pointer is
	// malformed (bad escape, bad array index).
	DiagInvalidExpression DiagnosticCode = "invalid_expression"
	// DiagSourceUnavailable reports that the expression refers to the request
	// or response (or a decoded body) that was not supplied to Eval.
	DiagSourceUnavailable DiagnosticCode = "source_unavailable"
	// DiagExpressionNoValue reports a well-formed expression whose value does
	// not exist: a missing header/query/path name, a body JSON Pointer that
	// does not resolve, or an empty URL.
	DiagExpressionNoValue DiagnosticCode = "expression_no_value"
	// DiagAmbiguousMultiValue reports a header/query source with several
	// scalar values that the target parameter cannot hold losslessly (its
	// schema is not an array). The first value is never picked silently.
	DiagAmbiguousMultiValue DiagnosticCode = "ambiguous_multi_value"
	// DiagAmbiguousParameterName reports an unqualified Link parameter name
	// that matches target parameters in more than one location.
	DiagAmbiguousParameterName DiagnosticCode = "ambiguous_parameter_name"
	// DiagInvalidParameterQualifier reports a qualified Link parameter name
	// whose "in" prefix is not one of path, query, header or cookie.
	DiagInvalidParameterQualifier DiagnosticCode = "invalid_parameter_qualifier"
	// DiagUnknownTargetParameter reports a Link parameter that does not match
	// any parameter declared by the target operation.
	DiagUnknownTargetParameter DiagnosticCode = "unknown_target_parameter"
	// DiagRequiredParameterMissing reports a required target parameter (or
	// request body) whose binding could not be produced. It always accompanies
	// the diagnostic that explains why the binding was omitted.
	DiagRequiredParameterMissing DiagnosticCode = "required_parameter_missing"
	// DiagSchemaMismatch reports a produced value that fails the target
	// parameter/request body schema. The value is still returned.
	DiagSchemaMismatch DiagnosticCode = "schema_mismatch"
)

// Diagnostic is a structured finding about one Link entry.
//
// Location is the evaluated entry: "parameters.<key>" for an entry of the
// Link's parameters object, or "requestBody". Expression is the runtime
// expression involved, empty when the entry is a constant or does not apply.
type Diagnostic struct {
	Location   string
	Code       DiagnosticCode
	Expression string
	Message    string
	// Cause is the underlying schema/pointer error, if any.
	Cause error
}

func (d Diagnostic) Error() string {
	if d.Expression != "" {
		return fmt.Sprintf("%s: %s (expression %q)", d.Location, d.Message, d.Expression)
	}
	return d.Location + ": " + d.Message
}

// runtimeExpr is a parsed OpenAPI runtime expression.
//
// Grammar (OpenAPI 3):
//
//	"$url" / "$method" / "$statusCode" /
//	"$request." source / "$response." source
//	source = header "." name / query "." name / path "." name / body [json-pointer]
type runtimeExpr struct {
	raw    string
	simple string // "url", "method", "statusCode"
	side   string // "request" or "response"
	source string // "header", "query", "path", "body"
	name   string // header/query/path parameter name
	// tokens are the decoded JSON Pointer tokens of a body reference; an
	// empty slice means the whole body.
	tokens     []string
	hasPointer bool
}

const (
	sideRequest  = "request"
	sideResponse = "response"
)

// parseRuntimeExpression parses s. The second result reports whether s looks
// like a runtime expression (it starts with "$"); when it does but violates
// the grammar, a non-nil error is returned.
func parseRuntimeExpression(s string) (*runtimeExpr, bool, error) {
	if !strings.HasPrefix(s, "$") {
		return nil, false, nil
	}
	switch s {
	case "$url":
		return &runtimeExpr{raw: s, simple: "url"}, true, nil
	case "$method":
		return &runtimeExpr{raw: s, simple: "method"}, true, nil
	case "$statusCode":
		return &runtimeExpr{raw: s, simple: "statusCode"}, true, nil
	}

	var side, rest string
	switch {
	case strings.HasPrefix(s, "$request."):
		side, rest = sideRequest, s[len("$request."):]
	case strings.HasPrefix(s, "$response."):
		side, rest = sideResponse, s[len("$response."):]
	default:
		return nil, true, fmt.Errorf("not a runtime expression: expected $url, $method, $statusCode, $request.* or $response.*")
	}
	if rest == "" {
		return nil, true, fmt.Errorf("missing source after $%s.", side)
	}

	e := &runtimeExpr{raw: s, side: side}

	// A "#" may only introduce a body JSON Pointer.
	namePart, pointerPart, hadHash := strings.Cut(rest, "#")
	if hadHash {
		e.hasPointer = true
		switch {
		case pointerPart == "":
			// "#" alone means the empty pointer (the whole body); leave
			// tokens empty but remember the marker.
		case !strings.HasPrefix(pointerPart, "/"):
			return nil, true, fmt.Errorf("JSON Pointer must start with '/' or be empty, got %q", pointerPart)
		default:
			tokens, err := parseJSONPointer(pointerPart)
			if err != nil {
				return nil, true, err
			}
			e.tokens = tokens
		}
	}

	source := namePart
	switch {
	case namePart == "body":
		e.source = "body"
	case strings.HasPrefix(namePart, "path.") && namePart != "path.":
		e.source, e.name = "path", namePart[len("path."):]
	case strings.HasPrefix(namePart, "query.") && namePart != "query.":
		e.source, e.name = "query", namePart[len("query."):]
	case strings.HasPrefix(namePart, "header.") && namePart != "header.":
		e.source, e.name = "header", namePart[len("header."):]
	default:
		return nil, true, fmt.Errorf("invalid source %q: expected body, path.<name>, query.<name> or header.<name>", source)
	}
	if side == sideResponse && (e.source == "path" || e.source == "query") {
		return nil, true, fmt.Errorf("$response only supports header and body sources, got %q", e.source)
	}
	if e.source != "body" && hadHash {
		return nil, true, fmt.Errorf("JSON Pointer is only allowed after body, got %s.%s#", side, e.source)
	}
	return e, true, nil
}

// parseJSONPointer parses an RFC 6901 JSON Pointer starting with "/". An
// empty string is the valid empty pointer.
func parseJSONPointer(p string) ([]string, error) {
	if p == "" {
		return nil, nil
	}
	if !strings.HasPrefix(p, "/") {
		return nil, fmt.Errorf("invalid JSON Pointer %q: must start with '/'", p)
	}
	rawTokens := strings.Split(p[1:], "/")
	tokens := make([]string, 0, len(rawTokens))
	for _, raw := range rawTokens {
		tok, err := unescapePointerToken(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid JSON Pointer %q: %w", p, err)
		}
		tokens = append(tokens, tok)
	}
	return tokens, nil
}

// unescapePointerToken decodes one reference token. Per RFC 6901, "~1" is
// decoded before "~0" and any other "~" occurrence is illegal.
func unescapePointerToken(token string) (string, error) {
	if !strings.ContainsRune(token, '~') {
		return token, nil
	}
	var b strings.Builder
	b.Grow(len(token))
	for i := 0; i < len(token); i++ {
		if token[i] != '~' {
			b.WriteByte(token[i])
			continue
		}
		if i+1 >= len(token) {
			return "", fmt.Errorf("unescaped '~' in token %q", token)
		}
		switch token[i+1] {
		case '0':
			b.WriteByte('~')
		case '1':
			b.WriteByte('/')
		default:
			return "", fmt.Errorf("invalid escape %q in token %q", token[i:i+2], token)
		}
		i++
	}
	return b.String(), nil
}

// pointerOutcome is the result of evaluating a header/query/path/body source.
type pointerOutcome struct {
	// values are the raw scalar occurrences (header/query).
	values []string
	// value is the single scalar, body value or whole-body value.
	value any
	// found reports existence of the value; an explicit JSON null is found
	// with value == nil.
	found bool
	// sourceMissing reports that the referenced side or decoded body was not
	// supplied at all, as opposed to a value being absent.
	sourceMissing bool
	// malformed reports that the body pointer contains an illegal token
	// (e.g. a leading-zero array index).
	malformed bool
}

// evalSource evaluates the expression against the supplied request/response
// state. req/resp may be nil when the caller did not supply that side.
func (e *runtimeExpr) eval(req *Request, resp *Response) pointerOutcome {
	switch e.simple {
	case "url":
		if req == nil || req.URL == nil {
			return pointerOutcome{}
		}
		s := req.URL.String()
		if s == "" {
			return pointerOutcome{}
		}
		return pointerOutcome{value: s, found: true}
	case "method":
		if req == nil || req.Method == "" {
			return pointerOutcome{}
		}
		return pointerOutcome{value: req.Method, found: true}
	case "statusCode":
		if resp == nil {
			return pointerOutcome{}
		}
		return pointerOutcome{value: resp.StatusCode, found: true}
	}

	if e.side == sideRequest && req == nil {
		return pointerOutcome{sourceMissing: true}
	}
	if e.side == sideResponse && resp == nil {
		return pointerOutcome{sourceMissing: true}
	}

	switch e.source {
	case "header":
		var h http.Header
		if e.side == sideRequest {
			h = req.Header
		} else {
			h = resp.Header
		}
		// http.Header keys are canonical, so Values performs a
		// case-insensitive lookup.
		values := h.Values(e.name)
		if len(values) == 0 {
			return pointerOutcome{}
		}
		return pointerOutcome{values: values, value: values[0], found: true}
	case "query":
		// Query parameter names are case-sensitive.
		values, ok := req.Query[e.name]
		if !ok || len(values) == 0 {
			return pointerOutcome{}
		}
		return pointerOutcome{values: values, value: values[0], found: true}
	case "path":
		// Path parameter names are case-sensitive.
		value, ok := req.PathParams[e.name]
		if !ok {
			return pointerOutcome{}
		}
		return pointerOutcome{value: value, found: true}
	case "body":
		var body any
		if e.side == sideRequest {
			body = req.Body
		} else {
			body = resp.Body
		}
		if body == nil {
			// The caller supplied no decoded body on this side. This differs
			// from a body that is present and contains a JSON null.
			return pointerOutcome{sourceMissing: true}
		}
		if !e.hasPointer {
			return pointerOutcome{value: body, found: true}
		}
		value, found, malformed := evalPointer(body, e.tokens)
		return pointerOutcome{value: value, found: found, malformed: malformed}
	}
	return pointerOutcome{}
}

// evalPointer walks a decoded JSON value following RFC 6901 reference
// tokens. found is false when a property is absent, an array index is out of
// range or a scalar is traversed. malformed is true when a token is not a
// legal array index (for example "01"). A property holding JSON null yields
// (nil, true, false).
func evalPointer(doc any, tokens []string) (value any, found bool, malformed bool) {
	cur := doc
	for _, token := range tokens {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[token]
			if !ok {
				return nil, false, false
			}
			cur = v
		case []any:
			idx, status := arrayIndex(token, len(node))
			switch status {
			case indexOK:
				cur = node[idx]
			case indexMalformed:
				return nil, false, true
			default: // indexMissing: out of range or "-"
				return nil, false, false
			}
		default:
			return nil, false, false
		}
	}
	return cur, true, false
}

type arrayIndexStatus int

const (
	indexOK arrayIndexStatus = iota
	indexMissing
	indexMalformed
)

// arrayIndex interprets token as an RFC 6901 array index: "0" or
// non-leading-zero base-10 digits in range are OK; "-" and an in-range-shaped
// index past the end is missing; leading zeros and non-numeric tokens are
// malformed.
func arrayIndex(token string, length int) (int, arrayIndexStatus) {
	if token == "-" || token == "" {
		return 0, indexMissing
	}
	for i := 0; i < len(token); i++ {
		if token[i] < '0' || token[i] > '9' {
			return 0, indexMalformed
		}
	}
	if len(token) > 1 && token[0] == '0' {
		// Leading zeros are illegal reference tokens.
		return 0, indexMalformed
	}
	idx, err := strconv.Atoi(token)
	if err != nil || idx < 0 || idx >= length {
		return 0, indexMissing
	}
	return idx, indexOK
}
