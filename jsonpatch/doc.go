// Package jsonpatch applies an RFC 6902 JSON Patch to a JSON resource and
// checks the resulting resource against a target openapi3.SchemaRef in the
// request direction.
//
// The entry points are [Apply] (for an already decoded resource and
// [Operation] sequence) and [ApplyJSON] (for a JSON encoded resource and
// patch document). They never modify the resource, the operations or the
// schema they receive: the patch is applied to a detached deep copy and it
// is only returned when every operation applied successfully and the final
// resource validates against the target schema. Defaults are never injected
// during that validation.
//
// This first version does not load external resources, does not touch HTTP
// requests, does not implement merge-patch (RFC 7386) and provides no
// database transactions.
package jsonpatch
