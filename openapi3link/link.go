package openapi3link

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-openapi/jsonpointer"
)

// Request is the current request, as observed by the caller.
//
// Eval reads only these fields. It never reads http.Request.Body: Body is the
// request payload already decoded by the caller and may be left nil when the
// Link does not reference $request.body.
type Request struct {
	// Method is the HTTP method of the request.
	Method string
	// URL is the full request URL. $url evaluates to its string form.
	URL *url.URL
	// Header holds the request headers. Lookup is case-insensitive.
	Header http.Header
	// PathParams holds path parameter values keyed by template name. Names are
	// case-sensitive.
	PathParams map[string]string
	// Query holds the parsed query string. Parameter names are case-sensitive.
	Query url.Values
	// Body is the already-decoded request body (map[string]any, []any, ...).
	Body any
}

// Response is the current response, as observed by the caller.
//
// Eval reads only these fields and never reads or closes an
// http.Response.Body. Body is the response payload already decoded by the
// caller.
type Response struct {
	// StatusCode is the response status code. $statusCode evaluates to it.
	StatusCode int
	// Header holds the response headers. Lookup is case-insensitive.
	Header http.Header
	// Body is the already-decoded response body (map[string]any, []any, ...).
	Body any
}

// Input is the input of Eval.
type Input struct {
	// Link is the Link Object to evaluate.
	Link *openapi3.Link
	// Documents is the set of already-loaded documents that target resolution
	// is allowed to use, keyed by each document's absolute base URI.
	// No document outside this set is ever accessed.
	Documents map[*url.URL]*openapi3.T
	// LinkDocumentURI is the base URI of the document containing Link.
	// operationRef values are resolved against it.
	LinkDocumentURI *url.URL

	// Request is the current request, when the Link references one.
	Request *Request
	// Response is the current response, when the Link references one.
	Response *Response
}

// Target identifies the operation the Link points at.
type Target struct {
	// DocumentURI is the base URI of the document declaring the operation.
	DocumentURI *url.URL
	// Path is the URL path template of the operation, e.g. "/users/{id}".
	Path string
	// Method is the HTTP method of the operation, e.g. "GET".
	Method string
	// Operation is the resolved target operation.
	Operation *openapi3.Operation
}

// ParameterBinding is one evaluated entry of the Link's parameters object.
type ParameterBinding struct {
	// Name is the target parameter name.
	Name string
	// In is the target parameter location: path, query, header or cookie.
	In string
	// Value is the evaluated value. JSON types are preserved: values taken
	// from a decoded body keep their original Go type, including nil for JSON
	// null.
	Value any
}

// Result is the output of Eval.
type Result struct {
	// Target is the resolved target operation.
	Target Target
	// Parameters are the parameter bindings produced for the target
	// operation, in the order the Link declares them.
	Parameters []ParameterBinding
	// RequestBody is the evaluated request body binding; meaningful only when
	// HasRequestBody is true.
	RequestBody any
	// HasRequestBody reports whether the Link produced a request body binding.
	HasRequestBody bool
	// Diagnostics are the non-fatal findings collected while evaluating the
	// Link (omitted bindings, ambiguities, schema mismatches, ...).
	Diagnostics []Diagnostic
}

// Eval resolves in.Link's target operation inside in.Documents and evaluates
// its parameter and request body bindings against in.Request/in.Response.
//
// Target resolution failures are returned as typed errors and a nil result.
// Per-entry problems (missing values, ambiguities, schema failures) do not
// abort evaluation: they are returned as Result.Diagnostics, and the affected
// binding is omitted (or retained for schema mismatches).
//
// Eval performs no I/O: it neither sends HTTP requests nor reads/closes any
// request or response body, and it never accesses documents outside
// in.Documents. It does not inject schema defaults and it does not mutate the
// documents, their schemas or the decoded bodies.
func Eval(ctx context.Context, in *Input) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in == nil {
		return nil, fmt.Errorf("nil link evaluation input")
	}
	if in.Link == nil {
		return nil, fmt.Errorf("nil link")
	}
	link := in.Link
	if link.OperationID != "" && link.OperationRef != "" {
		return nil, &BothTargetsError{OperationID: link.OperationID, OperationRef: link.OperationRef}
	}
	if link.OperationID == "" && link.OperationRef == "" {
		return nil, &NoTargetError{}
	}

	index := buildIndex(in.Documents)

	var loc opLocation
	if link.OperationRef != "" {
		var err error
		loc, err = resolveOperationRef(in.LinkDocumentURI, link.OperationRef, in.Documents, index)
		if err != nil {
			return nil, err
		}
	} else {
		locs := index.byID[link.OperationID]
		switch len(locs) {
		case 0:
			return nil, &OperationIDNotFoundError{OperationID: link.OperationID}
		case 1:
			loc = locs[0]
		default:
			return nil, &DuplicateOperationIDError{
				OperationID: link.OperationID,
				Locations:   locationsOf(locs),
			}
		}
	}

	targetPathItem := loc.doc.Paths.Value(loc.path)
	merged := mergeParameters(targetPathItem, loc.op)

	res := &Result{
		Target: Target{
			DocumentURI: loc.docURI,
			Path:        loc.path,
			Method:      loc.method,
			Operation:   loc.op,
		},
	}

	// Evaluate Link parameters in declaration order.
	for _, key := range sortedKeys(link.Parameters) {
		raw := link.Parameters[key]
		location := "parameters." + key
		params := findTargetParameters(merged, key)
		if params.qualifierInvalid {
			res.add(Diagnostic{
				Location: location,
				Code:     DiagInvalidParameterQualifier,
				Message:  fmt.Sprintf("parameter name %q has an invalid location qualifier", key),
			})
			continue
		}
		if len(params.matches) == 0 {
			res.add(Diagnostic{
				Location: location,
				Code:     DiagUnknownTargetParameter,
				Message:  fmt.Sprintf("target operation has no parameter named %q", key),
			})
			continue
		}
		var target *effectiveParameter
		if len(params.matches) > 1 {
			ins := make([]string, len(params.matches))
			for i, m := range params.matches {
				ins[i] = m.in + " " + m.name
			}
			res.add(Diagnostic{
				Location: location,
				Code:     DiagAmbiguousParameterName,
				Message: fmt.Sprintf("parameter name %q is declared in more than one location: %s",
					key, strings.Join(ins, ", ")),
			})
			if anyRequired(params.matches) {
				res.requiredMissing(location, "parameter", key)
			}
			continue
		}
		target = params.matches[0]

		value, bound, diag := evaluateValue(raw, in, "parameter "+target.in+" "+target.name)
		if diag != nil {
			diag.Location = location
			res.add(*diag)
		}
		if !bound {
			if target.param.Required {
				res.requiredMissing(location, "parameter", target.name)
			}
			continue
		}

		// Header/query multi-value handling: several occurrences are only
		// lossless when the target accepts an array.
		if len(value.sourceValues) > 1 && !acceptsArray(target.param) {
			res.add(Diagnostic{
				Location:   location,
				Code:       DiagAmbiguousMultiValue,
				Expression: value.expression,
				Message: fmt.Sprintf("%s %q has %d values but the target parameter is not an array; "+
					"refusing to pick one", target.in, target.name, len(value.sourceValues)),
			})
			if target.param.Required {
				res.requiredMissing(location, "parameter", target.name)
			}
			continue
		}

		boundValue := value.value
		if len(value.sourceValues) > 1 {
			arr := make([]any, len(value.sourceValues))
			for i, v := range value.sourceValues {
				arr[i] = v
			}
			boundValue = arr
		}

		if schema := parameterSchema(target.param); schema != nil {
			if err := validateValue(loc.doc, schema, boundValue); err != nil {
				res.add(Diagnostic{
					Location:   location,
					Code:       DiagSchemaMismatch,
					Expression: value.expression,
					Message:    fmt.Sprintf("value does not match the target parameter schema: %v", err),
					Cause:      err,
				})
			}
		}

		res.Parameters = append(res.Parameters, ParameterBinding{
			Name:  target.name,
			In:    target.in,
			Value: boundValue,
		})
	}

	// Evaluate the optional request body binding.
	if link.RequestBody != nil {
		value, bound, diag := evaluateValue(link.RequestBody, in, "request body")
		diagLocation := "requestBody"
		if diag != nil {
			diag.Location = diagLocation
			res.add(*diag)
		}
		if !bound {
			if loc.op.RequestBody != nil && loc.op.RequestBody.Value != nil && loc.op.RequestBody.Value.Required {
				res.requiredMissing(diagLocation, "request body", "")
			}
		} else {
			res.HasRequestBody = true
			res.RequestBody = value.value
			if schema := requestBodySchema(loc.op); schema != nil {
				if err := validateValue(loc.doc, schema, value.value); err != nil {
					res.add(Diagnostic{
						Location:   diagLocation,
						Code:       DiagSchemaMismatch,
						Expression: value.expression,
						Message:    fmt.Sprintf("value does not match the target request body schema: %v", err),
						Cause:      err,
					})
				}
			}
		}
	}

	return res, nil
}

// evaluatedValue is an interpreted Link parameter/request body entry.
type evaluatedValue struct {
	value        any
	sourceValues []string // raw header/query occurrences
	expression   string   // the runtime expression text, when it was one
}

// evaluateValue interprets one entry: constants pass through untouched,
// runtime expressions are evaluated. The returned diagnostic pointer is nil on
// success and its Location is set by the caller.
func evaluateValue(raw any, in *Input, what string) (evaluatedValue, bool, *Diagnostic) {
	s, isString := raw.(string)
	if !isString {
		// Non-string JSON values are constants.
		return evaluatedValue{value: raw}, true, nil
	}
	expr, isExpr, err := parseRuntimeExpression(s)
	if !isExpr {
		// Strings not starting with "$" are constants.
		return evaluatedValue{value: s}, true, nil
	}
	if err != nil {
		return evaluatedValue{}, false, &Diagnostic{
			Code:       DiagInvalidExpression,
			Expression: s,
			Message:    fmt.Sprintf("invalid runtime expression for %s: %v", what, err),
		}
	}
	out := expr.eval(in.Request, in.Response)
	switch {
	case out.sourceMissing:
		return evaluatedValue{}, false, &Diagnostic{
			Code:       DiagSourceUnavailable,
			Expression: s,
			Message:    fmt.Sprintf("expression %q references a %s that was not supplied", s, what),
		}
	case out.malformed:
		return evaluatedValue{}, false, &Diagnostic{
			Code:       DiagInvalidExpression,
			Expression: s,
			Message:    fmt.Sprintf("expression %q contains an invalid JSON Pointer token", s),
		}
	case !out.found:
		return evaluatedValue{}, false, &Diagnostic{
			Code:       DiagExpressionNoValue,
			Expression: s,
			Message:    fmt.Sprintf("expression %q has no value", s),
		}
	}
	return evaluatedValue{
		value:        out.value,
		sourceValues: out.values,
		expression:   s,
	}, true, nil
}

func (r *Result) add(d Diagnostic) {
	r.Diagnostics = append(r.Diagnostics, d)
}

func (r *Result) requiredMissing(location, kind, name string) {
	msg := "required target " + kind + " binding is missing"
	if name != "" {
		msg = fmt.Sprintf("required target %s %q binding is missing", kind, name)
	}
	r.add(Diagnostic{Location: location, Code: DiagRequiredParameterMissing, Message: msg})
}

// effectiveParameter is a target parameter after path-item/operation merge.
type effectiveParameter struct {
	param *openapi3.Parameter
	name  string
	in    string
}

type mergedParameters struct {
	byKey map[string]*effectiveParameter
	order []*effectiveParameter
}

// canonicalParamKey returns the merge/lookup key of a parameter location.
// Header names are folded to lower case because HTTP header names are
// case-insensitive; path/query/cookie names stay as declared.
func canonicalParamKey(in, name string) string {
	if in == openapi3.ParameterInHeader {
		return in + "\x00" + strings.ToLower(name)
	}
	return in + "\x00" + name
}

// mergeParameters merges path-item level parameters with operation level
// parameters. An operation parameter overrides a path-item parameter with the
// same name and location.
func mergeParameters(pathItem *openapi3.PathItem, op *openapi3.Operation) *mergedParameters {
	mp := &mergedParameters{byKey: map[string]*effectiveParameter{}}
	keyIndex := map[string]int{}
	add := func(ref *openapi3.ParameterRef) {
		if ref == nil || ref.Value == nil {
			return
		}
		p := ref.Value
		ep := &effectiveParameter{param: p, name: p.Name, in: p.In}
		key := canonicalParamKey(p.In, p.Name)
		if i, exists := keyIndex[key]; exists {
			// Operation parameter overrides the path-item one: replace the
			// merged entry in place so it is matched exactly once.
			mp.order[i] = ep
		} else {
			keyIndex[key] = len(mp.order)
			mp.order = append(mp.order, ep)
		}
		mp.byKey[key] = ep
	}
	if pathItem != nil {
		for _, ref := range pathItem.Parameters {
			add(ref)
		}
	}
	if op != nil {
		for _, ref := range op.Parameters {
			add(ref)
		}
	}
	return mp
}

type targetMatch struct {
	matches          []*effectiveParameter
	qualifierInvalid bool
}

// findTargetParameters resolves a Link parameter key to target parameters.
// Keys may be qualified ("path.id", "header.X-Foo"); an unqualified name
// matches in every location it is declared.
func findTargetParameters(mp *mergedParameters, key string) targetMatch {
	in := ""
	name := key
	if prefix, rest, ok := strings.Cut(key, "."); ok && isKnownIn(prefix) {
		in, name = prefix, rest
		if name == "" {
			return targetMatch{qualifierInvalid: true}
		}
	}
	var matches []*effectiveParameter
	if in != "" {
		if ep := mp.byKey[canonicalParamKey(in, name)]; ep != nil {
			matches = append(matches, ep)
		}
		return targetMatch{matches: matches}
	}
	for _, ep := range mp.order {
		if namesEqual(ep.in, ep.name, name) {
			matches = append(matches, ep)
		}
	}
	return targetMatch{matches: matches}
}

func isKnownIn(in string) bool {
	switch in {
	case openapi3.ParameterInPath, openapi3.ParameterInQuery,
		openapi3.ParameterInHeader, openapi3.ParameterInCookie:
		return true
	}
	return false
}

func namesEqual(in, declared, wanted string) bool {
	if in == openapi3.ParameterInHeader {
		return strings.EqualFold(declared, wanted)
	}
	return declared == wanted
}

func anyRequired(matches []*effectiveParameter) bool {
	for _, m := range matches {
		if m.param.Required {
			return true
		}
	}
	return false
}

func parameterSchema(p *openapi3.Parameter) *openapi3.Schema {
	if p == nil || p.Schema == nil {
		return nil
	}
	return p.Schema.Value
}

// acceptsArray reports whether a parameter schema can hold several scalar
// occurrences losslessly.
func acceptsArray(p *openapi3.Parameter) bool {
	s := parameterSchema(p)
	if s == nil {
		// Without a schema, nothing constrains the value: allow the array.
		return true
	}
	if s.Type != nil && s.Type.Includes(openapi3.TypeArray) {
		return true
	}
	// OpenAPI 3.0 style arrays sometimes rely on "items" alone.
	return s.Type.IsEmpty() && s.Items != nil
}

// requestBodySchema picks the schema to validate a Link request body binding
// against, preferring application/json.
func requestBodySchema(op *openapi3.Operation) *openapi3.Schema {
	if op == nil || op.RequestBody == nil || op.RequestBody.Value == nil {
		return nil
	}
	content := op.RequestBody.Value.Content
	if len(content) == 0 {
		return nil
	}
	keys := make([]string, 0, len(content))
	for k := range content {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pick := keys[0]
	for _, k := range keys {
		if strings.EqualFold(k, "application/json") {
			pick = k
			break
		}
	}
	mt := content[pick]
	if mt == nil || mt.Schema == nil {
		return nil
	}
	return mt.Schema.Value
}

// validateValue validates one produced binding against the target schema.
// Plain VisitJSON is used: it does not apply schema defaults, so neither the
// value nor the schema is mutated.
func validateValue(doc *openapi3.T, schema *openapi3.Schema, value any) error {
	if schema == nil {
		return nil
	}
	var opts []openapi3.SchemaValidationOption
	if doc != nil {
		opts = append(opts, doc.GetSchemaValidationOptions()...)
		if doc.IsOpenAPI31OrLater() {
			opts = append(opts, openapi3.EnableJSONSchema2020())
		}
	}
	return schema.VisitJSON(value, opts...)
}

// opLocation is one operation declaration in the provided document set.
type opLocation struct {
	doc    *openapi3.T
	docURI *url.URL
	path   string
	method string
	op     *openapi3.Operation
}

type docIndex struct {
	byID map[string][]opLocation
	byOP map[*openapi3.Operation][]opLocation
}

// buildIndex indexes every operation of every document. Documents, paths and
// methods are traversed in sorted order so that error output is deterministic.
func buildIndex(docs map[*url.URL]*openapi3.T) *docIndex {
	idx := &docIndex{
		byID: map[string][]opLocation{},
		byOP: map[*openapi3.Operation][]opLocation{},
	}
	docURIs := make([]*url.URL, 0, len(docs))
	for u := range docs {
		docURIs = append(docURIs, u)
	}
	sort.Slice(docURIs, func(i, j int) bool { return docURIs[i].String() < docURIs[j].String() })

	for _, docURI := range docURIs {
		doc := docs[docURI]
		if doc == nil || doc.Paths == nil {
			continue
		}
		for _, path := range doc.Paths.Keys() {
			pathItem := doc.Paths.Value(path)
			if pathItem == nil {
				continue
			}
			operations := pathItem.Operations()
			methods := make([]string, 0, len(operations))
			for m := range operations {
				methods = append(methods, m)
			}
			sort.Strings(methods)
			for _, method := range methods {
				op := operations[method]
				if op == nil {
					continue
				}
				entry := opLocation{doc: doc, docURI: docURI, path: path, method: strings.ToUpper(method), op: op}
				idx.byOP[op] = append(idx.byOP[op], entry)
				if op.OperationID != "" {
					idx.byID[op.OperationID] = append(idx.byID[op.OperationID], entry)
				}
			}
		}
	}
	return idx
}

func locationsOf(locs []opLocation) []TargetLocation {
	out := make([]TargetLocation, len(locs))
	for i, l := range locs {
		out[i] = TargetLocation{DocumentURI: l.docURI, Path: l.path, Method: l.method}
	}
	return out
}

// resolveOperationRef resolves an operationRef against the base URI of the
// document containing the Link and follows its JSON Pointer to the target
// operation. Only in.Documents may be consulted.
func resolveOperationRef(base *url.URL, operationRef string, docs map[*url.URL]*openapi3.T, idx *docIndex) (opLocation, error) {
	var zero opLocation
	if base == nil {
		return zero, &InvalidOperationRefError{
			OperationRef: operationRef,
			Reason:       "the URI of the document containing the link is required to resolve relative references",
		}
	}
	u, err := url.Parse(operationRef)
	if err != nil {
		return zero, &InvalidOperationRefError{OperationRef: operationRef, Reason: err.Error()}
	}
	if u.Fragment == "" {
		return zero, &InvalidOperationRefError{
			OperationRef: operationRef,
			Reason:       "an operationRef must contain a URI fragment identifying an operation",
		}
	}
	resolved := base.ResolveReference(u)

	doc, docURI, ok := findDocument(docs, resolved)
	if !ok {
		return zero, &ExternalDocumentNotProvidedError{OperationRef: operationRef, URI: withoutFragment(resolved)}
	}

	fragment := u.Fragment
	if fragment != "" && !strings.HasPrefix(fragment, "/") {
		return zero, &InvalidOperationRefError{
			OperationRef: operationRef,
			Reason:       fmt.Sprintf("URI fragment %q is not a JSON Pointer", fragment),
		}
	}
	pointer, err := jsonpointer.New(fragment)
	if err != nil {
		return zero, &InvalidOperationRefError{OperationRef: operationRef, Reason: err.Error()}
	}
	target, _, err := pointer.Get(doc)
	if err != nil {
		return zero, &OperationRefTargetNotFoundError{
			OperationRef: operationRef,
			DocumentURI:  docURI,
			Reason:       "JSON Pointer cannot be resolved: " + err.Error(),
		}
	}
	op, ok := target.(*openapi3.Operation)
	if !ok {
		return zero, &OperationRefTargetNotFoundError{
			OperationRef: operationRef,
			DocumentURI:  docURI,
			Reason:       fmt.Sprintf("JSON Pointer resolves to %T, not an Operation Object", target),
		}
	}

	locs := idx.byOP[op]
	var inDoc []opLocation
	for _, l := range locs {
		if l.doc == doc {
			inDoc = append(inDoc, l)
		}
	}
	switch len(inDoc) {
	case 0:
		return zero, &OperationRefTargetNotFoundError{
			OperationRef: operationRef,
			DocumentURI:  docURI,
			Reason:       "the referenced operation is not declared under a paths entry of the document",
		}
	case 1:
		return inDoc[0], nil
	default:
		return zero, &AmbiguousTargetError{OperationRef: operationRef, Locations: locationsOf(inDoc)}
	}
}

// findDocument looks up the document keyed by u's document URI (fragment
// ignored).
func findDocument(docs map[*url.URL]*openapi3.T, u *url.URL) (*openapi3.T, *url.URL, bool) {
	want := withoutFragment(u).String()
	var best *openapi3.T
	var bestURI *url.URL
	for docURI, doc := range docs {
		if withoutFragment(docURI).String() == want {
			best, bestURI = doc, docURI
			break
		}
	}
	return best, bestURI, best != nil
}

func withoutFragment(u *url.URL) *url.URL {
	c := *u
	c.Fragment = ""
	c.RawFragment = ""
	return &c
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
