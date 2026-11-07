package jsonpatch_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/jsonpatch"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/require"
)

// ---- helpers -----------------------------------------------------------------

func schemaRef(s *openapi3.Schema) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: s}
}

func decodeJSON(t *testing.T, data string) any {
	t.Helper()
	var v any
	dec := json.NewDecoder(strings.NewReader(data))
	dec.UseNumber()
	require.NoError(t, dec.Decode(&v))
	return v
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// applyJSON applies a patch document to a JSON resource and requires success.
func applyJSON(t *testing.T, schema *openapi3.SchemaRef, resource, patch string, opts ...jsonpatch.Option) any {
	t.Helper()
	result, err := jsonpatch.ApplyJSON(schema, []byte(resource), []byte(patch), opts...)
	require.NoError(t, err)
	return result
}

func requireResultJSON(t *testing.T, expected string, got any) {
	t.Helper()
	require.JSONEq(t, expected, mustJSON(t, got))
}

func patchErr(t *testing.T, err error) jsonpatch.Error {
	t.Helper()
	var pe jsonpatch.Error
	require.ErrorAs(t, err, &pe)
	return pe
}

func objectWithIDSchema() *openapi3.Schema {
	s := openapi3.NewObjectSchema()
	s.Properties["id"] = schemaRef(openapi3.NewStringSchema())
	s.Properties["name"] = schemaRef(openapi3.NewStringSchema())
	s.Required = []string{"id"}
	return s
}

func integerArraySchema() *openapi3.Schema {
	s := openapi3.NewArraySchema()
	s.Items = schemaRef(openapi3.NewIntegerSchema())
	return s
}

// ---- required scenarios ------------------------------------------------------

func TestApply_ArrayMoveForwardAndBackward(t *testing.T) {
	s := schemaRef(integerArraySchema())

	// RFC 6902 canonical vectors: the target index counts on the array after
	// the source element was removed.
	got := applyJSON(t, s, `[1,2,3,4]`, `[{"op":"move","from":"/1","path":"/2"}]`)
	requireResultJSON(t, `[1,3,2,4]`, got)

	got = applyJSON(t, s, `[1,2,3,4]`, `[{"op":"move","from":"/2","path":"/1"}]`)
	requireResultJSON(t, `[1,3,2,4]`, got)

	// Move to the tail: after removing index 0 the array has length 3, so
	// index 3 appends.
	got = applyJSON(t, s, `[1,2,3,4]`, `[{"op":"move","from":"/0","path":"/3"}]`)
	requireResultJSON(t, `[2,3,4,1]`, got)

	// Move to the head.
	got = applyJSON(t, s, `[1,2,3,4]`, `[{"op":"move","from":"/3","path":"/0"}]`)
	requireResultJSON(t, `[4,1,2,3]`, got)
}

func TestApply_CopyThenModifySource(t *testing.T) {
	s := openapi3.NewObjectSchema()
	s.Properties["a"] = schemaRef(objectWithIDSchema())
	s.Properties["b"] = schemaRef(objectWithIDSchema())
	ref := schemaRef(s)

	got := applyJSON(t, ref,
		`{"a":{"id":"1"},"y":2}`,
		`[
			{"op":"copy","from":"/a","path":"/b"},
			{"op":"replace","path":"/a/id","value":"9"},
			{"op":"add","path":"/a/extra","value":"x"}
		]`)
	requireResultJSON(t, `{"a":{"id":"9","extra":"x"},"b":{"id":"1"},"y":2}`, got)

	// Copying an array into itself inserts an independent element.
	arrSchema := schemaRef(integerArraySchema())
	got = applyJSON(t, arrSchema, `[1,2,3]`, `[{"op":"copy","from":"/1","path":"/1"}]`)
	requireResultJSON(t, `[1,2,2,3]`, got)
	got = applyJSON(t, arrSchema, `[1,2,3]`, `[
		{"op":"copy","from":"/0","path":"/-"},
		{"op":"replace","path":"/0","value":9}
	]`)
	requireResultJSON(t, `[9,2,3,1]`, got)
}

func TestApply_CopyDoesNotAliasPatchValue(t *testing.T) {
	s := schemaRef(objectWithIDSchema())
	value := map[string]any{"id": "x"}
	ops := []jsonpatch.Operation{jsonpatch.NewAddOperation("/nested", value)}
	resource := map[string]any{"id": "root"}

	got, err := jsonpatch.Apply(s, resource, ops)
	require.NoError(t, err)

	// Mutating the value carried by the operation and the input resource
	// must not affect the returned resource.
	value["id"] = "mutated"
	resource["id"] = "mutated"
	requireResultJSON(t, `{"id":"root","nested":{"id":"x"}}`, got)

	// And mutating the result must not reach back into the inputs.
	got.(map[string]any)["nested"].(map[string]any)["id"] = "other"
	require.Equal(t, "mutated", value["id"])
}

func TestApply_RootNullAndRootRemoval(t *testing.T) {
	nullable := objectWithIDSchema()
	nullable.Nullable = true
	s := schemaRef(nullable)

	// Replacing the root with JSON null succeeds on a nullable schema; a nil
	// result means the JSON resource is null.
	got := applyJSON(t, s, `{"id":"a"}`, `[{"op":"replace","path":"","value":null}]`)
	require.Nil(t, got)

	// test against the null root compares against null, not "missing".
	got = applyJSON(t, s, `null`, `[{"op":"test","path":"","value":null}]`)
	require.Nil(t, got)

	// Removing the root leaves no resource to validate: the final schema
	// validation fails rather than treating the absence as null.
	_, err := jsonpatch.ApplyJSON(s, []byte(`{"id":"a"}`),
		[]byte(`[{"op":"remove","path":""}]`))
	pe := patchErr(t, err)
	require.Equal(t, jsonpatch.KindSchema, pe.Kind())
	require.Equal(t, "", pe.Pointer())

	// An intermediate root-less state is representable: remove then re-add a
	// conforming root in a later operation succeeds.
	got = applyJSON(t, s, `{"id":"a"}`, `[
		{"op":"remove","path":""},
		{"op":"add","path":"","value":{"id":"b"}}
	]`)
	requireResultJSON(t, `{"id":"b"}`, got)

	// Adding a member while the root is absent targets nothing.
	_, err = jsonpatch.ApplyJSON(s, []byte(`{"id":"a"}`), []byte(`[
		{"op":"remove","path":""},
		{"op":"add","path":"/x","value":1}
	]`))
	pe = patchErr(t, err)
	require.Equal(t, jsonpatch.KindTargetNotFound, pe.Kind())
	require.Equal(t, 1, pe.OperationIndex())
	require.Equal(t, "/x", pe.Pointer())

	// Root removal followed by root null succeeds on the nullable schema.
	got = applyJSON(t, s, `{"id":"a"}`, `[
		{"op":"remove","path":""},
		{"op":"add","path":"","value":null}
	]`)
	require.Nil(t, got)

	// A non-nullable schema rejects the final null resource at validation.
	_, err = jsonpatch.ApplyJSON(schemaRef(objectWithIDSchema()), []byte(`{"id":"a"}`),
		[]byte(`[{"op":"replace","path":"","value":null}]`))
	pe = patchErr(t, err)
	require.Equal(t, jsonpatch.KindSchema, pe.Kind())
}

func TestApply_RemoveThenReAddRequiredProperty(t *testing.T) {
	s := schemaRef(objectWithIDSchema())

	// Missing "id" mid-patch is acceptable; the final document is valid again.
	got := applyJSON(t, s, `{"id":"a","name":"b"}`, `[
		{"op":"remove","path":"/id"},
		{"op":"add","path":"/id","value":"c"}
	]`)
	requireResultJSON(t, `{"id":"c","name":"b"}`, got)

	// When the required property stays missing, the whole patch fails as a
	// final schema error and no resource is returned.
	result, err := jsonpatch.ApplyJSON(s, []byte(`{"id":"a"}`),
		[]byte(`[{"op":"remove","path":"/id"}]`))
	require.Nil(t, result)
	pe := patchErr(t, err)
	require.Equal(t, jsonpatch.KindSchema, pe.Kind())
	require.Equal(t, "/id", pe.Pointer())
}

func TestApply_TestBigIntegersAndNumberEquality(t *testing.T) {
	s := openapi3.NewObjectSchema()
	s.Properties["a"] = schemaRef(openapi3.NewIntegerSchema())
	ref := schemaRef(s)

	// 1 and 1.0 are equal JSON numbers.
	applyJSON(t, ref, `{"a":1}`, `[{"op":"test","path":"/a","value":1.0}]`)
	applyJSON(t, ref, `{"a":1.0}`, `[{"op":"test","path":"/a","value":1}]`)

	// A large integer equals itself.
	applyJSON(t, ref, `{"a":9007199254740992}`,
		`[{"op":"test","path":"/a","value":9007199254740992}]`)

	// Two consecutive integers beyond the float64 exact-integer range must
	// NOT compare equal.
	_, err := jsonpatch.ApplyJSON(ref, []byte(`{"a":9007199254740992}`),
		[]byte(`[{"op":"test","path":"/a","value":9007199254740993}]`))
	pe := patchErr(t, err)
	require.Equal(t, jsonpatch.KindTestFailure, pe.Kind())
	require.Equal(t, 0, pe.OperationIndex())
	require.Equal(t, "/a", pe.Pointer())
	var tf *jsonpatch.TestFailureError
	require.ErrorAs(t, err, &tf)
	require.Equal(t, json.Number("9007199254740993"), tf.Expected)
	require.Equal(t, json.Number("9007199254740992"), tf.Actual)

	// Equality is structural for objects and arrays too.
	applyJSON(t, schemaRef(openapi3.NewObjectSchema()),
		`{"a":[1,1.0,{"b":2}]}`,
		`[{"op":"test","path":"","value":{"a":[1.0,1,{"b":2.00}]}}]`)
}

func TestApply_ReadOnlyPropertyRejected(t *testing.T) {
	s := openapi3.NewObjectSchema()
	id := openapi3.NewStringSchema()
	id.ReadOnly = true
	s.Properties["id"] = schemaRef(id)
	s.Properties["name"] = schemaRef(openapi3.NewStringSchema())
	ref := schemaRef(s)

	result, err := jsonpatch.ApplyJSON(ref, []byte(`{"name":"x"}`),
		[]byte(`[{"op":"add","path":"/id","value":"42"}]`))
	require.Nil(t, result)
	pe := patchErr(t, err)
	require.Equal(t, jsonpatch.KindSchema, pe.Kind())
	require.Equal(t, "/id", pe.Pointer())

	// readOnly validation can be disabled through schema options if wanted.
	applyJSON(t, ref, `{"name":"x"}`,
		`[{"op":"add","path":"/id","value":"42"}]`,
		jsonpatch.WithSchemaValidationOptions(openapi3.DisableReadOnlyValidation()))
}

func TestApply_InputsUnchangedAfterLastStepFailure(t *testing.T) {
	s := schemaRef(objectWithIDSchema())

	resource := decodeJSON(t, `{"id":"a","n":1}`)
	before := mustJSON(t, resource)

	ops := []jsonpatch.Operation{
		jsonpatch.NewAddOperation("/extra", map[string]any{"k": json.Number("7")}),
		jsonpatch.NewTestOperation("/n", json.Number("5")), // fails: 1 != 5
	}
	opsSnapshot := mustJSON(t, ops)

	result, err := jsonpatch.Apply(s, resource, ops)
	require.Nil(t, result)
	pe := patchErr(t, err)
	require.Equal(t, jsonpatch.KindTestFailure, pe.Kind())
	require.Equal(t, 1, pe.OperationIndex())

	// Neither the resource nor the operations were mutated.
	require.Equal(t, before, mustJSON(t, resource))
	require.Equal(t, opsSnapshot, mustJSON(t, ops))
	require.NotContains(t, before, "extra")

	// Same guarantee through the JSON entry point.
	result, err = jsonpatch.ApplyJSON(s,
		[]byte(`{"id":"a","n":1}`),
		[]byte(`[
			{"op":"add","path":"/extra","value":{"k":7}},
			{"op":"test","path":"/n","value":5}
		]`))
	require.Nil(t, result)
	pe = patchErr(t, err)
	require.Equal(t, jsonpatch.KindTestFailure, pe.Kind())
}

// ---- operation semantics -----------------------------------------------------

func TestApply_ObjectAddReplaceRemoveAndEscapes(t *testing.T) {
	s := openapi3.NewObjectSchema()
	s.AdditionalProperties = openapi3.AdditionalProperties{Has: openapi3.BoolPtr(true)}
	ref := schemaRef(s)

	got := applyJSON(t, ref, `{"a/b":1,"m~n":2,"":3}`, `[
		{"op":"test","path":"/a~1b","value":1},
		{"op":"test","path":"/m~0n","value":2},
		{"op":"test","path":"/","value":3},
		{"op":"add","path":"/","value":30},
		{"op":"replace","path":"/a~1b","value":10},
		{"op":"remove","path":"/m~0n"}
	]`)
	requireResultJSON(t, `{"a/b":10,"":30}`, got)

	// replace and remove require an existing target.
	for _, op := range []string{
		`{"op":"replace","path":"/missing","value":1}`,
		`{"op":"remove","path":"/missing"}`,
		`{"op":"test","path":"/missing","value":1}`,
	} {
		_, err := jsonpatch.ApplyJSON(ref, []byte(`{}`), []byte("["+op+"]"))
		pe := patchErr(t, err)
		require.Equal(t, jsonpatch.KindTargetNotFound, pe.Kind(), op)
		require.Equal(t, "/missing", pe.Pointer(), op)
	}
}

func TestApply_ArrayAddSpliceAndDash(t *testing.T) {
	s := schemaRef(integerArraySchema())

	got := applyJSON(t, s, `[1,2,3]`, `[
		{"op":"add","path":"/-","value":4},
		{"op":"add","path":"/0","value":0}
	]`)
	requireResultJSON(t, `[0,1,2,3,4]`, got)

	got = applyJSON(t, s, `[1,2,3]`, `[{"op":"add","path":"/1","value":9}]`)
	requireResultJSON(t, `[1,9,2,3]`, got)

	// add past the end (not "-", not an existing slot) is invalid.
	_, err := jsonpatch.ApplyJSON(s, []byte(`[1,2,3]`),
		[]byte(`[{"op":"add","path":"/9","value":4}]`))
	require.Equal(t, jsonpatch.KindSyntax, patchErr(t, err).Kind())
}

func TestApply_MoveAcrossContainersAndSelf(t *testing.T) {
	s := openapi3.NewObjectSchema()
	s.AdditionalProperties = openapi3.AdditionalProperties{Has: openapi3.BoolPtr(true)}
	ref := schemaRef(s)

	got := applyJSON(t, ref, `{"a":[1,2],"b":{}}`, `[
		{"op":"move","from":"/a/1","path":"/b/x"}
	]`)
	requireResultJSON(t, `{"a":[1],"b":{"x":2}}`, got)

	// Moving a location to the same location is a no-op.
	got = applyJSON(t, ref, `{"a":[1,2],"b":{}}`, `[{"op":"move","from":"/a/1","path":"/a/1"}]`)
	requireResultJSON(t, `{"a":[1,2],"b":{}}`, got)

	// Moving into a descendant is impossible.
	_, err := jsonpatch.ApplyJSON(ref, []byte(`{"a":{"b":1}}`),
		[]byte(`[{"op":"move","from":"/a","path":"/a/b"}]`))
	pe := patchErr(t, err)
	require.Equal(t, jsonpatch.KindSyntax, pe.Kind())
	require.Equal(t, 0, pe.OperationIndex())

	// Moving the whole root under one of its members is impossible.
	_, err = jsonpatch.ApplyJSON(ref, []byte(`{"a":1}`),
		[]byte(`[{"op":"move","from":"","path":"/x"}]`))
	require.Equal(t, jsonpatch.KindSyntax, patchErr(t, err).Kind())

	// Moving a member up to the root works.
	got = applyJSON(t, schemaRef(openapi3.NewIntegerSchema()),
		`{"a":1}`, `[{"op":"move","from":"/a","path":""}]`)
	requireResultJSON(t, `1`, got)

	// move/copy require an existing source.
	_, err = jsonpatch.ApplyJSON(ref, []byte(`{}`),
		[]byte(`[{"op":"move","from":"/missing","path":"/x"}]`))
	pe = patchErr(t, err)
	require.Equal(t, jsonpatch.KindTargetNotFound, pe.Kind())
	require.Equal(t, "from", pe.Field())
	require.Equal(t, "/missing", pe.Pointer())
}

func TestApply_TestFailureStopsThePatch(t *testing.T) {
	s := schemaRef(objectWithIDSchema())
	// Op 0 fails; op 1 (which would add a property) must not be applied.
	_, err := jsonpatch.ApplyJSON(s, []byte(`{"id":"a"}`), []byte(`[
		{"op":"test","path":"/id","value":"b"},
		{"op":"add","path":"/x","value":1}
	]`))
	pe := patchErr(t, err)
	require.Equal(t, jsonpatch.KindTestFailure, pe.Kind())
	require.Equal(t, 0, pe.OperationIndex())

	got := applyJSON(t, s, `{"id":"a"}`, `[
		{"op":"test","path":"/id","value":"a"},
		{"op":"add","path":"/name","value":"b"}
	]`)
	requireResultJSON(t, `{"id":"a","name":"b"}`, got)
}

// ---- syntax and limit errors -------------------------------------------------

func TestApply_SyntaxErrors(t *testing.T) {
	objectSchema := schemaRef(openapi3.NewObjectSchema())
	arraySchema := schemaRef(integerArraySchema())
	cases := []struct {
		name     string
		resource string
		schema   *openapi3.SchemaRef
		patch    string
		opIndex  int
		pointer  string
	}{
		{"not an array", `{}`, objectSchema, `{"op":"add"}`, -1, ""},
		{"missing op", `{}`, objectSchema, `[{"path":"/a","value":1}]`, 0, ""},
		{"unknown op", `{}`, objectSchema, `[{"op":"frobnicate","path":"/a","value":1}]`, 0, ""},
		{"missing path", `{}`, objectSchema, `[{"op":"remove"}]`, 0, ""},
		{"add missing value", `{}`, objectSchema, `[{"op":"add","path":"/a"}]`, 0, ""},
		{"move missing from", `{}`, objectSchema, `[{"op":"move","path":"/a"}]`, 0, ""},
		{"copy missing from", `{}`, objectSchema, `[{"op":"copy","path":"/a"}]`, 0, ""},
		{"pointer without slash", `{}`, objectSchema, `[{"op":"remove","path":"a"}]`, 0, "a"},
		{"bad escape", `{}`, objectSchema, `[{"op":"remove","path":"/a~2b"}]`, 0, "/a~2b"},
		{"leading zero index", `[1]`, arraySchema, `[{"op":"remove","path":"/01"}]`, 0, "/01"},
		{"dash out of range", `[1]`, arraySchema, `[{"op":"add","path":"/5","value":2}]`, 0, "/5"},
		{"malformed json", `{}`, objectSchema, `[{"op":`, -1, ""},
		{"trailing document", `{}`, objectSchema, `[{}] {}`, -1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := jsonpatch.ApplyJSON(tc.schema, []byte(tc.resource), []byte(tc.patch))
			pe := patchErr(t, err)
			require.Equal(t, jsonpatch.KindSyntax, pe.Kind(), err.Error())
			require.Equal(t, tc.opIndex, pe.OperationIndex())
			if tc.pointer != "" {
				require.Equal(t, tc.pointer, pe.Pointer())
			}
		})
	}

	// "-" names the (nonexistent) member after the last array element, so for
	// remove/replace/test it is a missing target, not a pointer syntax error.
	for _, op := range []string{
		`{"op":"remove","path":"/-"}`,
		`{"op":"replace","path":"/-","value":2}`,
		`{"op":"test","path":"/-","value":2}`,
	} {
		_, err := jsonpatch.ApplyJSON(arraySchema, []byte(`[1]`), []byte("["+op+"]"))
		pe := patchErr(t, err)
		require.Equal(t, jsonpatch.KindTargetNotFound, pe.Kind(), op)
		require.Equal(t, "/-", pe.Pointer(), op)
	}

	// An empty patch is valid and leaves the resource untouched.
	got := applyJSON(t, schemaRef(objectWithIDSchema()), `{"id":"a"}`, `[]`)
	requireResultJSON(t, `{"id":"a"}`, got)

	// "01" and "-" are legitimate object member names, so against an object
	// they are ordinary missing-member (or add) operations.
	got = applyJSON(t, objectSchema, `{}`, `[{"op":"add","path":"/01","value":"x"}]`)
	requireResultJSON(t, `{"01":"x"}`, got)
}

func TestApply_PointersParsedInOperationOrder(t *testing.T) {
	s := schemaRef(objectWithIDSchema())
	// The first operation fails its test, so the malformed pointer of the
	// later operation is never reached.
	_, err := jsonpatch.ApplyJSON(s, []byte(`{"id":"a"}`), []byte(`[
		{"op":"test","path":"/id","value":"b"},
		{"op":"remove","path":"a"}
	]`))
	pe := patchErr(t, err)
	require.Equal(t, jsonpatch.KindTestFailure, pe.Kind())
	require.Equal(t, 0, pe.OperationIndex())

	// Once the test passes, the later syntax error surfaces.
	_, err = jsonpatch.ApplyJSON(s, []byte(`{"id":"a"}`), []byte(`[
		{"op":"test","path":"/id","value":"a"},
		{"op":"remove","path":"a"}
	]`))
	pe = patchErr(t, err)
	require.Equal(t, jsonpatch.KindSyntax, pe.Kind())
	require.Equal(t, 1, pe.OperationIndex())
}

func TestApply_Limits(t *testing.T) {
	s := schemaRef(objectWithIDSchema())

	_, err := jsonpatch.ApplyJSON(s, []byte(`{"id":"a"}`), []byte(`[
		{"op":"test","path":"/id","value":"a"},
		{"op":"test","path":"/id","value":"a"}
	]`), jsonpatch.WithMaxOperations(1))
	pe := patchErr(t, err)
	require.Equal(t, jsonpatch.KindSyntax, pe.Kind())
	require.Equal(t, -1, pe.OperationIndex())

	_, err = jsonpatch.ApplyJSON(s, []byte(`{"id":"a"}`),
		[]byte(`[{"op":"test","path":"/id/x","value":1}]`),
		jsonpatch.WithMaxPointerDepth(1))
	pe = patchErr(t, err)
	require.Equal(t, jsonpatch.KindSyntax, pe.Kind())
	require.Equal(t, 0, pe.OperationIndex())
	require.Equal(t, "/id/x", pe.Pointer())
}

// ---- validation behaviour ----------------------------------------------------

func TestApply_DefaultsAreNotInjected(t *testing.T) {
	s := openapi3.NewObjectSchema()
	withDefault := openapi3.NewStringSchema()
	withDefault.Default = "generated"
	s.Properties["x"] = schemaRef(withDefault)
	ref := schemaRef(s)

	got := applyJSON(t, ref, `{}`, `[]`)
	requireResultJSON(t, `{}`, got)
}

func TestApply_NullRootResource(t *testing.T) {
	nullable := openapi3.NewObjectSchema()
	nullable.Nullable = true
	got := applyJSON(t, schemaRef(nullable), `null`, `[]`)
	require.Nil(t, got)

	_, err := jsonpatch.ApplyJSON(schemaRef(openapi3.NewObjectSchema()), []byte(`null`), []byte(`[]`))
	require.Equal(t, jsonpatch.KindSchema, patchErr(t, err).Kind())
}

func TestApply_UnresolvedSchemaRef(t *testing.T) {
	_, err := jsonpatch.Apply(&openapi3.SchemaRef{Ref: "#/components/schemas/X"}, map[string]any{}, nil)
	require.ErrorIs(t, err, jsonpatch.ErrUnresolvedTargetSchema)

	_, err = jsonpatch.Apply(nil, map[string]any{}, nil)
	require.ErrorIs(t, err, jsonpatch.ErrUnresolvedTargetSchema)
}

func TestApply_SchemaErrorWrapsOpenAPIError(t *testing.T) {
	s := schemaRef(objectWithIDSchema())
	_, err := jsonpatch.ApplyJSON(s, []byte(`{"id":"a"}`),
		[]byte(`[{"op":"remove","path":"/id"}]`))
	var se *openapi3.SchemaError
	require.ErrorAs(t, err, &se, "the openapi3 schema error must remain available via errors.As")
	require.Equal(t, []string{"id"}, se.JSONPointer())
}

func TestApply_WrongTypeIsFinalSchemaError(t *testing.T) {
	s := schemaRef(openapi3.NewStringSchema())
	_, err := jsonpatch.ApplyJSON(s, []byte(`{"a":1}`), []byte(`[]`))
	pe := patchErr(t, err)
	require.Equal(t, jsonpatch.KindSchema, pe.Kind())
}

// Field is exposed on operation-level errors to identify the failing member.
func TestApply_ErrorFields(t *testing.T) {
	var se *jsonpatch.SyntaxError
	_, err := jsonpatch.ApplyJSON(schemaRef(openapi3.NewObjectSchema()), []byte(`{}`),
		[]byte(`[{"op":"move","path":"/a"}]`))
	require.ErrorAs(t, err, &se)
	require.Equal(t, "from", se.Field())
}

func TestUnmarshalPatch(t *testing.T) {
	ops, err := jsonpatch.UnmarshalPatch([]byte(`[
		{"op":"add","path":"/a","value":1,"comment":"extra member is ignored"}
	]`))
	require.NoError(t, err)
	require.Len(t, ops, 1)
	require.Equal(t, "add", ops[0].Op)
	require.Equal(t, json.Number("1"), ops[0].Value)

	_, err = jsonpatch.UnmarshalPatch([]byte(`{"not":"an array"}`))
	var se *jsonpatch.SyntaxError
	require.ErrorAs(t, err, &se)

	// A Go-built nil Value for add means JSON null.
	s := openapi3.NewObjectSchema()
	s.AdditionalProperties = openapi3.AdditionalProperties{Has: openapi3.BoolPtr(true)}
	got := applyJSON(t, schemaRef(s), `{}`, `[]`)
	requireResultJSON(t, `{}`, got)
	result, err := jsonpatch.Apply(schemaRef(s), map[string]any{},
		[]jsonpatch.Operation{jsonpatch.NewAddOperation("/x", nil)})
	require.NoError(t, err)
	requireResultJSON(t, `{"x":null}`, result)

	// Sanity guard against accidental API drift: all error kinds are distinct.
	require.NotEqual(t, jsonpatch.KindSyntax, jsonpatch.KindTargetNotFound)
	require.NotEqual(t, jsonpatch.KindTestFailure, jsonpatch.KindSchema)
}
