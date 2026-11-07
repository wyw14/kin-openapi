package jsonpatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// Operation is one RFC 6902 patch operation: add, remove, replace, move, copy
// or test.
//
// When building operations in Go, From is used only by move and copy and
// Value only by add, replace and test. A nil Value is the JSON value null.
// Operations decoded from a JSON patch document additionally track whether
// the mandatory "path", "from" and "value" members were present.
type Operation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	From  string `json:"from,omitempty"`
	Value any    `json:"value,omitempty"`

	// Presence flags, populated only by UnmarshalJSON so that an operation
	// decoded without a required member fails instead of root "" / null being
	// mistaken for an explicitly given member.
	hasPath  bool
	hasFrom  bool
	hasValue bool
}

// NewAddOperation builds an "add" operation.
func NewAddOperation(path string, value any) Operation {
	return Operation{Op: "add", Path: path, Value: value, hasPath: true, hasValue: true}
}

// NewRemoveOperation builds a "remove" operation.
func NewRemoveOperation(path string) Operation {
	return Operation{Op: "remove", Path: path, hasPath: true}
}

// NewReplaceOperation builds a "replace" operation.
func NewReplaceOperation(path string, value any) Operation {
	return Operation{Op: "replace", Path: path, Value: value, hasPath: true, hasValue: true}
}

// NewMoveOperation builds a "move" operation.
func NewMoveOperation(from, path string) Operation {
	return Operation{Op: "move", Path: path, From: from, hasPath: true, hasFrom: true}
}

// NewCopyOperation builds a "copy" operation.
func NewCopyOperation(from, path string) Operation {
	return Operation{Op: "copy", Path: path, From: from, hasPath: true, hasFrom: true}
}

// NewTestOperation builds a "test" operation.
func NewTestOperation(path string, value any) Operation {
	return Operation{Op: "test", Path: path, Value: value, hasPath: true, hasValue: true}
}

// UnmarshalJSON decodes one RFC 6902 operation object, recording member
// presence. Numbers in Value are decoded with json.Number. Unknown members
// (such as "comment" in the RFC test vectors) are ignored.
func (op *Operation) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if raw, ok := fields["op"]; ok {
		if isJSONNull(raw) {
			return fmt.Errorf(`member "op" must be a JSON string, got null`)
		}
		if err := json.Unmarshal(raw, &op.Op); err != nil {
			return fmt.Errorf(`member "op" must be a JSON string: %w`, err)
		}
	}
	if raw, ok := fields["path"]; ok {
		if isJSONNull(raw) {
			return fmt.Errorf(`member "path" must be a JSON string, got null`)
		}
		if err := json.Unmarshal(raw, &op.Path); err != nil {
			return fmt.Errorf(`member "path" must be a JSON string: %w`, err)
		}
		op.hasPath = true
	}
	if raw, ok := fields["from"]; ok {
		if isJSONNull(raw) {
			return fmt.Errorf(`member "from" must be a JSON string, got null`)
		}
		if err := json.Unmarshal(raw, &op.From); err != nil {
			return fmt.Errorf(`member "from" must be a JSON string: %w`, err)
		}
		op.hasFrom = true
	}
	if raw, ok := fields["value"]; ok {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var value any
		if err := dec.Decode(&value); err != nil {
			return fmt.Errorf(`member "value" must be a JSON value: %w`, err)
		}
		op.Value = value
		op.hasValue = true
	}
	return nil
}

// isJSONNull reports whether raw encodes the JSON null literal.
func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// UnmarshalPatch decodes an RFC 6902 patch document (a JSON array of
// operations). Only the JSON syntax and member types are checked here;
// operations are validated in order while applying, so an earlier failing
// operation (for example a failing test) is reported before a malformed later
// one.
func UnmarshalPatch(data []byte) ([]Operation, error) {
	var ops []Operation
	if err := decodeSingleJSONValue(bytes.NewReader(data), &ops); err != nil {
		return nil, &SyntaxError{opError: opError{
			opIndex: -1,
			reason:  "patch document must be a JSON array of patch operations",
			cause:   err,
		}}
	}
	if ops == nil {
		// "[]" decodes to a non-nil empty slice; nil here means the decoded
		// value was JSON null rather than an array.
		return nil, &SyntaxError{opError: opError{opIndex: -1, reason: "patch document must be a JSON array, got null"}}
	}
	return ops, nil
}

// Apply applies ops to a detached deep copy of resource and, when every
// operation applied successfully, validates the result against schema in the
// request direction. None of resource, ops and schema are modified. The
// patched resource is returned only on full success, so a nil result with a
// nil error means the final JSON resource is legitimately null.
func Apply(schema *openapi3.SchemaRef, resource any, ops []Operation, opts ...Option) (any, error) {
	o := evalOptions(opts...)

	if schema == nil || schema.Value == nil {
		ref := ""
		if schema != nil {
			ref = schema.Ref
		}
		return nil, fmt.Errorf("%w (ref %q): use a SchemaRef resolved by openapi3.Loader", ErrUnresolvedTargetSchema, ref)
	}
	if len(ops) > o.maxOperations {
		return nil, &SyntaxError{opError: opError{
			opIndex: -1,
			reason:  fmt.Sprintf("patch has %d operations, which exceeds the limit of %d", len(ops), o.maxOperations),
		}}
	}

	doc := &document{root: deepCopyJSONValue(resource), hasRoot: true}
	for i, op := range ops {
		if err := applyOperation(doc, i, op, o); err != nil {
			return nil, err
		}
	}

	if !doc.hasRoot {
		return nil, &SchemaValidationError{
			pointer: "",
			reason:  "the patch removes the root resource and does not re-add it, so there is no value to validate against the target schema",
		}
	}

	validationOpts := make([]openapi3.SchemaValidationOption, 0, len(o.schemaValidationOptions)+1)
	// Request direction enforces readOnly constraints. DefaultsSet is never
	// passed, so validation cannot inject "default" values into the resource.
	validationOpts = append(validationOpts, openapi3.VisitAsRequest())
	validationOpts = append(validationOpts, o.schemaValidationOptions...)
	if err := schema.Value.VisitJSON(doc.root, validationOpts...); err != nil {
		return nil, &SchemaValidationError{pointer: schemaErrorPointer(err), cause: err}
	}

	return doc.root, nil
}

// ApplyJSON decodes resourceJSON and patchJSON with json.Number numbers and
// applies them as described on [Apply]. JSON null is a valid resource root.
func ApplyJSON(schema *openapi3.SchemaRef, resourceJSON, patchJSON []byte, opts ...Option) (any, error) {
	var resource any
	if err := decodeSingleJSONValue(bytes.NewReader(resourceJSON), &resource); err != nil {
		return nil, &SyntaxError{opError: opError{
			opIndex: -1,
			reason:  "target resource must be a single JSON value",
			cause:   err,
		}}
	}
	ops, err := UnmarshalPatch(patchJSON)
	if err != nil {
		return nil, err
	}
	return Apply(schema, resource, ops, opts...)
}

func decodeSingleJSONValue(r io.Reader, target any) error {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if err := dec.Decode(target); err != nil {
		return err
	}
	// Reject trailing data: both inputs must be exactly one JSON document.
	var probe json.RawMessage
	if err := dec.Decode(&probe); err != io.EOF {
		if err == nil {
			return errors.New("unexpected data after the JSON value")
		}
		return err
	}
	return nil
}

// document is the patch working copy. hasRoot is explicit so that the absence
// of any root (after {"op":"remove","path":""}) is never conflated with a
// root whose JSON value is null.
type document struct {
	root    any
	hasRoot bool
}

func applyOperation(doc *document, index int, op Operation, o *options) error {
	// "op" is validated first, like a parser would.
	switch op.Op {
	case "add", "remove", "replace", "move", "copy", "test":
	default:
		if op.Op == "" {
			return syntaxErrorf(index, "op", "", "operation is missing the %q member", "op")
		}
		return syntaxErrorf(index, "op", "", "unknown operation %q: allowed values are add, remove, replace, move, copy, test", op.Op)
	}
	if !op.hasPath {
		return syntaxErrorf(index, "path", "", "operation is missing the %q member", "path")
	}
	path, err := parsePointer(op.Path, o.maxPointerDepth)
	if err != nil {
		return syntaxErrorf(index, "path", op.Path, "%s", err.Error())
	}

	switch op.Op {
	case "add":
		if !op.hasValue {
			return syntaxErrorf(index, "value", "", "add operation is missing the %q member", "value")
		}
		return addValue(doc, index, op.Path, path, deepCopyJSONValue(op.Value))
	case "remove":
		return removeValue(doc, index, "path", op.Path, path)
	case "replace":
		if !op.hasValue {
			return syntaxErrorf(index, "value", "", "replace operation is missing the %q member", "value")
		}
		return replaceValue(doc, index, op.Path, path, deepCopyJSONValue(op.Value))
	case "test":
		if !op.hasValue {
			return syntaxErrorf(index, "value", "", "test operation is missing the %q member", "value")
		}
		return testValue(doc, index, op.Path, path, op.Value)
	case "move":
		if !op.hasFrom {
			return syntaxErrorf(index, "from", "", "move operation is missing the %q member", "from")
		}
		from, err := parsePointer(op.From, o.maxPointerDepth)
		if err != nil {
			return syntaxErrorf(index, "from", op.From, "%s", err.Error())
		}
		return moveValue(doc, index, op.From, from, op.Path, path)
	case "copy":
		if !op.hasFrom {
			return syntaxErrorf(index, "from", "", "copy operation is missing the %q member", "from")
		}
		from, err := parsePointer(op.From, o.maxPointerDepth)
		if err != nil {
			return syntaxErrorf(index, "from", op.From, "%s", err.Error())
		}
		return copyValue(doc, index, op.From, from, op.Path, path)
	}
	return nil
}

// resolve follows the tokens from the root. The boolean distinguishes a found
// JSON null from a missing value. A non-nil error is a malformed array
// reference token (KindSyntax); an ordinary missing value returns false.
func (doc *document) resolve(tokens jsonPointer) (value any, found bool, err error) {
	if !doc.hasRoot {
		return nil, false, nil
	}
	cur := doc.root
	for _, token := range tokens {
		switch v := cur.(type) {
		case map[string]any:
			child, ok := v[token]
			if !ok {
				return nil, false, nil
			}
			cur = child
		case []any:
			idx, ierr := parseArrayIndex(token, len(v), false)
			if ierr != nil {
				if ate, ok := ierr.(*arrayTokenError); ok && ate.malformed {
					return nil, false, ate
				}
				return nil, false, nil
			}
			cur = v[idx]
		default:
			return nil, false, nil
		}
	}
	return cur, true, nil
}

// parentLocation resolves the container addressed by tokens[:len(tokens)-1]
// together with the last reference token. setContainer stores the container
// back where it came from, which is required after an array reslices. A
// non-nil err is a malformed array token in the parent chain; ok=false means
// the parent simply does not exist or is not a container.
func (doc *document) parentLocation(tokens jsonPointer) (container any, token string, setContainer func(any), ok bool, err error) {
	if len(tokens) == 0 {
		return nil, "", nil, false, nil
	}
	switch len(tokens) {
	case 1:
		if !doc.hasRoot {
			return nil, "", nil, false, nil
		}
		container = doc.root
		setContainer = func(v any) { doc.root = v }
	default:
		parentTokens := tokens[:len(tokens)-1]
		last := parentTokens[len(parentTokens)-1]
		grand, gtoken, _, found, gerr := doc.parentLocation(parentTokens)
		if gerr != nil {
			return nil, "", nil, false, gerr
		}
		if !found {
			return nil, "", nil, false, nil
		}
		switch gp := grand.(type) {
		case map[string]any:
			child, exists := gp[gtoken]
			if !exists {
				return nil, "", nil, false, nil
			}
			container = child
			setContainer = func(v any) { gp[gtoken] = v }
		case []any:
			idx, ierr := parseArrayIndex(last, len(gp), false)
			if ierr != nil {
				if ate, ok := ierr.(*arrayTokenError); ok && ate.malformed {
					return nil, "", nil, false, ate
				}
				return nil, "", nil, false, nil
			}
			container = gp[idx]
			setContainer = func(v any) { gp[idx] = v }
		default:
			return nil, "", nil, false, nil
		}
	}
	switch container.(type) {
	case map[string]any, []any:
	default:
		return nil, "", nil, false, nil
	}
	return container, tokens[len(tokens)-1], setContainer, true, nil
}

func addValue(doc *document, index int, rawPath string, path jsonPointer, value any) error {
	if len(path) == 0 {
		doc.root, doc.hasRoot = value, true
		return nil
	}
	container, token, set, ok, err := doc.parentLocation(path)
	if err != nil {
		return syntaxErrorf(index, "path", rawPath, "%s", err.Error())
	}
	if !ok {
		return targetNotFoundErrorf(index, "path", rawPath, "cannot add %q: its containing object or array does not exist", token)
	}
	switch c := container.(type) {
	case map[string]any:
		c[token] = value
	case []any:
		idx, ierr := parseArrayIndex(token, len(c), true)
		if ierr != nil {
			return syntaxErrorf(index, "path", rawPath, "%s", ierr.Error())
		}
		c = append(c, nil)
		copy(c[idx+1:], c[idx:])
		c[idx] = value
		set(c)
	}
	return nil
}

func removeValue(doc *document, index int, field, rawPath string, path jsonPointer) error {
	if len(path) == 0 {
		if !doc.hasRoot {
			return targetNotFoundErrorf(index, field, rawPath, "cannot remove the root: the document has no root value")
		}
		doc.root, doc.hasRoot = nil, false
		return nil
	}
	container, token, set, ok, err := doc.parentLocation(path)
	if err != nil {
		return syntaxErrorf(index, field, rawPath, "%s", err.Error())
	}
	if !ok {
		return targetNotFoundErrorf(index, field, rawPath, "cannot remove %q: target does not exist", token)
	}
	switch c := container.(type) {
	case map[string]any:
		if _, exists := c[token]; !exists {
			return targetNotFoundErrorf(index, field, rawPath, "cannot remove missing object member %q", token)
		}
		delete(c, token)
	case []any:
		idx, ierr := parseArrayIndex(token, len(c), false)
		if ierr != nil {
			if ate, ok := ierr.(*arrayTokenError); ok && ate.malformed {
				return syntaxErrorf(index, field, rawPath, "%s", ierr.Error())
			}
			return targetNotFoundErrorf(index, field, rawPath, "cannot remove: %s", ierr.Error())
		}
		c = append(c[:idx], c[idx+1:]...)
		set(c)
	}
	return nil
}

func replaceValue(doc *document, index int, rawPath string, path jsonPointer, value any) error {
	if len(path) == 0 {
		if !doc.hasRoot {
			return targetNotFoundErrorf(index, "path", rawPath, "cannot replace the root: the document has no root value")
		}
		doc.root = value
		return nil
	}
	container, token, _, ok, err := doc.parentLocation(path)
	if err != nil {
		return syntaxErrorf(index, "path", rawPath, "%s", err.Error())
	}
	if !ok {
		return targetNotFoundErrorf(index, "path", rawPath, "cannot replace %q: target does not exist", token)
	}
	switch c := container.(type) {
	case map[string]any:
		if _, exists := c[token]; !exists {
			return targetNotFoundErrorf(index, "path", rawPath, "cannot replace missing object member %q", token)
		}
		c[token] = value
	case []any:
		idx, ierr := parseArrayIndex(token, len(c), false)
		if ierr != nil {
			if ate, ok := ierr.(*arrayTokenError); ok && ate.malformed {
				return syntaxErrorf(index, "path", rawPath, "%s", ierr.Error())
			}
			return targetNotFoundErrorf(index, "path", rawPath, "cannot replace: %s", ierr.Error())
		}
		c[idx] = value
	}
	return nil
}

func testValue(doc *document, index int, rawPath string, path jsonPointer, expected any) error {
	actual, found, err := doc.resolve(path)
	if err != nil {
		return syntaxErrorf(index, "path", rawPath, "%s", err.Error())
	}
	if !found {
		return targetNotFoundErrorf(index, "path", rawPath, "cannot test: target does not exist")
	}
	if !jsonEqual(expected, actual) {
		return &TestFailureError{
			opError: opError{
				opIndex: index,
				field:   "path",
				pointer: rawPath,
				reason:  "test failed: the target value is not equal to the expected value",
			},
			Expected: expected,
			Actual:   actual,
		}
	}
	return nil
}

func moveValue(doc *document, index int, rawFrom string, from jsonPointer, rawPath string, path jsonPointer) error {
	// Moving a location into one of its own descendants is impossible. Equal
	// pointers are a no-op once the source is known to exist.
	if isProperPrefix(from, path) {
		return syntaxErrorf(index, "path", rawPath, "cannot move %q into its own descendant %q", rawFrom, rawPath)
	}
	value, found, err := doc.resolve(from)
	if err != nil {
		return syntaxErrorf(index, "from", rawFrom, "%s", err.Error())
	}
	if !found {
		return targetNotFoundErrorf(index, "from", rawFrom, "cannot move: source does not exist")
	}
	if equalPointer(from, path) {
		return nil
	}
	// move is remove followed by add. The target pointer is evaluated against
	// the document after the removal, so array target indices count on the
	// shortened array.
	if err := removeValue(doc, index, "from", rawFrom, from); err != nil {
		return err
	}
	return addValue(doc, index, rawPath, path, value)
}

func copyValue(doc *document, index int, rawFrom string, from jsonPointer, rawPath string, path jsonPointer) error {
	value, found, err := doc.resolve(from)
	if err != nil {
		return syntaxErrorf(index, "from", rawFrom, "%s", err.Error())
	}
	if !found {
		return targetNotFoundErrorf(index, "from", rawFrom, "cannot copy: source does not exist")
	}
	// Composite values are copied by value: editing the original afterwards
	// must not change the copy.
	return addValue(doc, index, rawPath, path, deepCopyJSONValue(value))
}

func equalPointer(a, b jsonPointer) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// deepCopyJSONValue returns a value detached from v for the JSON data model
// produced by encoding/json with UseNumber: nil, bool, string, json.Number,
// float64, map[string]any and []any. Scalars are immutable and returned as is.
func deepCopyJSONValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case map[string]any:
		clone := make(map[string]any, len(x))
		for k, val := range x {
			clone[k] = deepCopyJSONValue(val)
		}
		return clone
	case []any:
		clone := make([]any, len(x))
		for i, val := range x {
			clone[i] = deepCopyJSONValue(val)
		}
		return clone
	default:
		return v
	}
}

// jsonEqual compares two values by their JSON value semantics. Numbers are
// compared mathematically (1 and 1.0 are equal) via exact rationals, so two
// distinct large integers are not collapsed the way float64 would.
func jsonEqual(a, b any) bool {
	if ra, aok := asRat(a); aok {
		rb, bok := asRat(b)
		return bok && ra.Cmp(rb) == 0
	}
	switch av := a.(type) {
	case nil:
		return b == nil
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			w, exists := bv[k]
			if !exists || !jsonEqual(v, w) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}

// asRat reports whether v is a JSON number and returns its exact rational
// value.
func asRat(v any) (*big.Rat, bool) {
	switch n := v.(type) {
	case json.Number:
		r, ok := new(big.Rat).SetString(string(n))
		if !ok {
			// Every number produced by encoding/json parses here; keep a
			// float fallback for hand-built values.
			f, err := n.Float64()
			if err != nil {
				return nil, false
			}
			r := new(big.Rat).SetFloat64(f)
			return r, r != nil
		}
		return r, true
	case float64:
		r := new(big.Rat).SetFloat64(n)
		return r, r != nil
	case float32:
		r := new(big.Rat).SetFloat64(float64(n))
		return r, r != nil
	case int:
		return new(big.Rat).SetInt64(int64(n)), true
	case int8:
		return new(big.Rat).SetInt64(int64(n)), true
	case int16:
		return new(big.Rat).SetInt64(int64(n)), true
	case int32:
		return new(big.Rat).SetInt64(int64(n)), true
	case int64:
		return new(big.Rat).SetInt64(n), true
	case uint:
		return new(big.Rat).SetUint64(uint64(n)), true
	case uint8:
		return new(big.Rat).SetUint64(uint64(n)), true
	case uint16:
		return new(big.Rat).SetUint64(uint64(n)), true
	case uint32:
		return new(big.Rat).SetUint64(uint64(n)), true
	case uint64:
		return new(big.Rat).SetUint64(n), true
	default:
		return nil, false
	}
}

// schemaErrorPointer extracts the RFC 6901 pointer of the underlying schema
// error, "" when the root failed or no structured schema error is available.
func schemaErrorPointer(err error) string {
	var se *openapi3.SchemaError
	if errors.As(err, &se) {
		return pointerTokensToJSONPointer(se.JSONPointer())
	}
	var me openapi3.MultiError
	if errors.As(err, &me) {
		for _, e := range me {
			if p := schemaErrorPointer(e); p != "" {
				return p
			}
		}
	}
	return ""
}

func pointerTokensToJSONPointer(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	var b strings.Builder
	for _, token := range tokens {
		b.WriteByte('/')
		b.WriteString(encodePointerToken(token))
	}
	return b.String()
}
