package openapi3filter

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// Styled parameter encoding, the counterpart of decodeStyledParameter.
//
// The encoder turns an already-parsed parameter value into typed fragments
// that callers can place into an HTTP request. It never assembles a whole URL
// and never mutates a request or the parameter definition.
//
// Percent-encoding is applied to every atomic value BEFORE the atomic values
// are joined with style delimiters, so delimiter characters found in the data
// (',' ';' '.' '=' '/', ...) can never become structural characters. Input is
// never percent-decoded, so an existing '%' in the data is encoded once.

// EncodingErrorKind discriminates the failures returned by EncodeParameter.
type EncodingErrorKind int

const (
	// EncodingErrorUnsupported means the value/style/explode/location
	// combination has no serialization defined by the OpenAPI styles, for
	// example a null value, a nested array under deepObject, or an encoding
	// declared via the "content" property.
	EncodingErrorUnsupported EncodingErrorKind = iota
	// EncodingErrorInvalid means the value did not pass the parameter's JSON
	// schema. Defaults are never applied to paper over such a mismatch.
	EncodingErrorInvalid
	// EncodingErrorIllegal means the value contains bytes that cannot be
	// legally expressed at the parameter location, for example CR/LF in a
	// header value or a comma in a cookie value.
	EncodingErrorIllegal
	// EncodingErrorConflict means two parameters claim ownership of the same
	// query key when several parameters are encoded together.
	EncodingErrorConflict
)

// Sentinel errors that an *EncodingError matches with errors.Is.
var (
	ErrUnsupportedEncoding  = errors.New("unsupported parameter encoding")
	ErrInvalidValue         = errors.New("invalid parameter value")
	ErrKeyOwnershipConflict = errors.New("query key ownership conflict")
)

// EncodingError is the single typed error returned by the parameter encoder.
// Inspect Kind to distinguish unsupported combinations, schema violations,
// illegal bytes and query key ownership conflicts.
type EncodingError struct {
	Kind      EncodingErrorKind
	Parameter *openapi3.Parameter
	// OtherParameter is the parameter that already owned a query key when
	// Kind is EncodingErrorConflict.
	OtherParameter *openapi3.Parameter
	Style          string
	Explode        bool
	// Key is the logical (not percent-encoded) query key of a conflict.
	Key   string
	Value any
	// Path locates a nested value (slice indexes and property names).
	Path []any

	Reason string
	Cause  error
}

var (
	_ error                       = (*EncodingError)(nil)
	_ interface{ Unwrap() error } = (*EncodingError)(nil)
)

func (e *EncodingError) Error() string {
	var b strings.Builder
	if e.Parameter != nil {
		fmt.Fprintf(&b, "parameter %q in %s (style=%q, explode=%t)", e.Parameter.Name, e.Parameter.In, e.Style, e.Explode)
	} else {
		b.WriteString("parameter encoding")
	}
	if len(e.Path) > 0 {
		parts := make([]string, 0, len(e.Path))
		for _, p := range e.Path {
			parts = append(parts, fmt.Sprint(p))
		}
		fmt.Fprintf(&b, " at %s", strings.Join(parts, "."))
	}
	if e.Reason != "" {
		b.WriteString(": ")
		b.WriteString(e.Reason)
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

func (e *EncodingError) Unwrap() error {
	if e.Cause != nil {
		return e.Cause
	}
	switch e.Kind {
	case EncodingErrorInvalid:
		return ErrInvalidValue
	case EncodingErrorConflict:
		return ErrKeyOwnershipConflict
	default:
		return ErrUnsupportedEncoding
	}
}

// Is lets errors.Is recognize the kind-level sentinels. An illegal value has
// no defined representation, so it matches ErrUnsupportedEncoding too.
func (e *EncodingError) Is(target error) bool {
	switch target {
	case ErrInvalidValue:
		return e.Kind == EncodingErrorInvalid
	case ErrKeyOwnershipConflict:
		return e.Kind == EncodingErrorConflict
	case ErrUnsupportedEncoding:
		return e.Kind == EncodingErrorUnsupported || e.Kind == EncodingErrorIllegal
	}
	return false
}

func newEncodingError(kind EncodingErrorKind, param *openapi3.Parameter, sm *openapi3.SerializationMethod, value any, reason string, cause error) *EncodingError {
	e := &EncodingError{Kind: kind, Parameter: param, Value: value, Reason: reason, Cause: cause}
	if sm != nil {
		e.Style, e.Explode = sm.Style, sm.Explode
	}
	return e
}

// EncodedQueryPair is one key/value pair of an encoded query string.
//
// Key and Value use the query component's percent-encoding: they are ready to
// be concatenated with '=' and '&' into url.URL.RawQuery. They MUST NOT be
// handed to url.Values.Add/Set, which would percent-encode them a second
// time.
//
// The sequence is ordered rather than a map because an exploded array and an
// exploded object emit several pairs, repeated keys are legal, and their
// order is significant.
type EncodedQueryPair struct {
	Key   string
	Value string
}

// EncodedPathParameter is the encoding of an in:path parameter: Fragment
// replaces the "{Name}" template in the URL path. It already contains the
// style prefix (simple: none, label: ".", matrix: ";name=") and percent
// encoding, including "%2F" for slashes found in the data.
type EncodedPathParameter struct {
	Name     string
	Fragment string
}

// EncodedHeaderParameter is the encoding of an in:header parameter, which is
// always the "simple" style: a single raw (not percent-encoded) field value.
type EncodedHeaderParameter struct {
	Name  string
	Value string
}

// EncodedCookieParameter is the encoding of an in:cookie parameter, which is
// always the non-exploded "form" style: a single raw cookie-octet value to be
// sent as "Name=Value" in a Cookie header.
type EncodedCookieParameter struct {
	Name  string
	Value string
}

// EncodedParameter is the styled encoding of one parameter. Exactly one of
// its Path, Query, Header or Cookie fields is non-nil/non-empty according to
// the parameter's "in".
type EncodedParameter struct {
	Parameter *openapi3.Parameter

	Path   *EncodedPathParameter
	Query  []EncodedQueryPair
	Header *EncodedHeaderParameter
	Cookie *EncodedCookieParameter
}

// ParameterValue binds an already-parsed value to a parameter for encoding.
// Values follow the JSON data model: nil, bool, string, the built-in numeric
// types or encoding/json.Number, []any and map[string]any.
type ParameterValue struct {
	Parameter *openapi3.Parameter
	Value     any
}

// EncodedRequestParameters groups encodings produced for several parameters.
// Slices preserve the order in which parameters were supplied; query pairs
// keep repeated keys.
type EncodedRequestParameters struct {
	Path   []EncodedPathParameter
	Query  []EncodedQueryPair
	Header []EncodedHeaderParameter
	Cookie []EncodedCookieParameter
}

// EncodeParameter serializes value according to the parameter's schema and
// its style/explode.
//
// The value is first checked against the parameter's JSON schema; schema
// defaults are never injected. The supported value shapes are the ones the
// OpenAPI styles define: primitives, arrays of primitives and flat objects
// with primitive properties for simple/label/matrix (path), form/
// spaceDelimited/pipeDelimited/deepObject (query), simple (header) and form
// (cookie). Combinations without a definition (null values, deepObject
// nested arrays or objects, cookie arrays with explode, "content"
// parameters, ...) return an *EncodingError with Kind
// EncodingErrorUnsupported; the encoder never falls back to fmt.Sprint or to
// a JSON string.
func EncodeParameter(param *openapi3.Parameter, value any) (*EncodedParameter, error) {
	if param == nil {
		return nil, newEncodingError(EncodingErrorUnsupported, nil, nil, value, "parameter is nil", nil)
	}
	sm, err := param.SerializationMethod()
	if err != nil {
		return nil, newEncodingError(EncodingErrorUnsupported, param, nil, value, err.Error(), err)
	}

	if len(param.Content) > 0 {
		return nil, newEncodingError(EncodingErrorUnsupported, param, sm, value,
			`encoding parameters defined with the "content" property is not supported`, nil)
	}
	if param.Schema == nil || param.Schema.Value == nil {
		return nil, newEncodingError(EncodingErrorUnsupported, param, sm, value,
			"a schema is required to encode a parameter value", nil)
	}

	// Check the value against the schema first. This never writes defaults
	// back into the value (see VisitJSON).
	if verr := param.Schema.Value.VisitJSON(value); verr != nil {
		return nil, newEncodingError(EncodingErrorInvalid, param, sm, value,
			"value does not conform to the parameter schema", verr)
	}

	out := &EncodedParameter{Parameter: param}
	switch param.In {
	case openapi3.ParameterInPath:
		fragment, err := encodePathFragment(param, sm, value)
		if err != nil {
			return nil, err
		}
		out.Path = &EncodedPathParameter{Name: param.Name, Fragment: fragment}
	case openapi3.ParameterInQuery:
		pairs, err := encodeQueryPairs(param, sm, value)
		if err != nil {
			return nil, err
		}
		out.Query = pairs
	case openapi3.ParameterInHeader:
		hv, err := encodeHeaderValue(param, sm, value)
		if err != nil {
			return nil, err
		}
		out.Header = &EncodedHeaderParameter{Name: param.Name, Value: hv}
	case openapi3.ParameterInCookie:
		cv, err := encodeCookieValue(param, sm, value)
		if err != nil {
			return nil, err
		}
		out.Cookie = &EncodedCookieParameter{Name: param.Name, Value: cv}
	default:
		return nil, newEncodingError(EncodingErrorUnsupported, param, sm, value,
			fmt.Sprintf("unsupported parameter's 'in': %q", param.In), nil)
	}
	return out, nil
}

// EncodeParameters encodes several parameters and groups their fragments.
//
// Query pairs are appended in parameter order with repeated keys preserved.
// When two parameters claim the same logical query key (as an exploded form
// object does with each of its property keys, and a deepObject parameter
// does with its "name[prop]" keys), an *EncodingError with Kind
// EncodingErrorConflict is returned instead of one parameter silently
// overwriting another.
func EncodeParameters(params ...ParameterValue) (*EncodedRequestParameters, error) {
	out := &EncodedRequestParameters{}
	owners := make(map[string]*openapi3.Parameter)

	for _, pv := range params {
		enc, err := EncodeParameter(pv.Parameter, pv.Value)
		if err != nil {
			return nil, err
		}
		if enc.Path != nil {
			out.Path = append(out.Path, *enc.Path)
		}
		if enc.Header != nil {
			out.Header = append(out.Header, *enc.Header)
		}
		if enc.Cookie != nil {
			out.Cookie = append(out.Cookie, *enc.Cookie)
		}
		for _, pair := range enc.Query {
			// Ownership is tracked on the logical (decoded) key, the way a
			// server sees it after parsing the query, so raw and percent
			// encoded forms of the same key cannot hide a conflict.
			logicalKey, perr := url.QueryUnescape(pair.Key)
			if perr != nil {
				return nil, newEncodingError(EncodingErrorInvalid, pv.Parameter, nil, pv.Value,
					"invalid percent-encoding in generated query key", perr)
			}
			if owner, ok := owners[logicalKey]; ok && owner != pv.Parameter {
				return nil, &EncodingError{
					Kind:           EncodingErrorConflict,
					Parameter:      pv.Parameter,
					OtherParameter: owner,
					Key:            logicalKey,
					Value:          pv.Value,
					Reason: fmt.Sprintf("query key %q is already owned by parameter %q",
						logicalKey, owner.Name),
				}
			}
			owners[logicalKey] = pv.Parameter
			out.Query = append(out.Query, pair)
		}
	}
	return out, nil
}

// ---- value shapes -------------------------------------------------------

type valueShape int

const (
	shapeScalar valueShape = iota
	shapeArray
	shapeObject
)

func shapeOf(value any) (valueShape, bool) {
	switch value.(type) {
	case nil:
		return shapeScalar, false
	case string, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, json.Number:
		return shapeScalar, true
	case []any:
		return shapeArray, true
	case map[string]any:
		return shapeObject, true
	default:
		return shapeScalar, false
	}
}

// scalarString renders an atomic value the same way the decoder parses it:
// booleans as "true"/"false", integers/numbers in decimal notation and
// strings verbatim. It refuses null, non-finite floats and unsupported Go
// types instead of silently using fmt.Sprint.
func scalarString(value any, path []any) (string, error) {
	switch v := value.(type) {
	case nil:
		return "", newEncodingError(EncodingErrorUnsupported, nil, nil, value,
			"null has no serialization representation in the parameter styles", nil).withPath(path)
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case json.Number:
		return v.String(), nil
	case int:
		return strconv.FormatInt(int64(v), 10), nil
	case int8:
		return strconv.FormatInt(int64(v), 10), nil
	case int16:
		return strconv.FormatInt(int64(v), 10), nil
	case int32:
		return strconv.FormatInt(int64(v), 10), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case uint:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint8:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint64:
		return strconv.FormatUint(v, 10), nil
	case float32:
		return floatString(float64(v), 32, path)
	case float64:
		return floatString(v, 64, path)
	default:
		return "", newEncodingError(EncodingErrorUnsupported, nil, nil, value,
			fmt.Sprintf("values of type %T are not supported", value), nil).withPath(path)
	}
}

func floatString(f float64, bitSize int, path []any) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", newEncodingError(EncodingErrorUnsupported, nil, nil, f,
			"NaN and infinite numbers have no parameter serialization", nil).withPath(path)
	}
	return strconv.FormatFloat(f, 'f', -1, bitSize), nil
}

func (e *EncodingError) withPath(path []any) *EncodingError {
	e.Path = slices.Clone(path)
	return e
}

// arrayScalarStrings renders array items, requiring every item to be an
// atomic value: the OpenAPI styles do not define nested arrays or objects.
func arrayScalarStrings(items []any) ([]string, error) {
	out := make([]string, len(items))
	for i, item := range items {
		s, err := scalarString(item, []any{i})
		if err != nil {
			return nil, err
		}
		out[i] = s
	}
	return out, nil
}

type objectEntry struct {
	name  string
	value string
}

// objectScalarEntries renders object property names and values in a stable
// alphabetical order, requiring every property value to be atomic: the
// first version of the encoder supports flat objects only.
func objectScalarEntries(props map[string]any) ([]objectEntry, error) {
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	slices.Sort(names)
	entries := make([]objectEntry, 0, len(names))
	for _, name := range names {
		v, err := scalarString(props[name], []any{name})
		if err != nil {
			return nil, err
		}
		entries = append(entries, objectEntry{name: name, value: v})
	}
	return entries, nil
}

// ---- percent-escaping ---------------------------------------------------

func isUnreservedByte(b byte) bool {
	return b == '-' || b == '.' || b == '_' || b == '~' ||
		(b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// appendPercentEncoded appends the percent-encoded UTF-8 bytes of s. Bytes
// for which allowed returns true are kept verbatim. A space is encoded as
// '+' when plusForSpace is true (query component), otherwise as "%20".
func appendPercentEncoded(b *strings.Builder, s string, allowed func(byte) bool, plusForSpace bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case allowed(c):
			b.WriteByte(c)
		case plusForSpace && c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			const hex = "0123456789ABCDEF"
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
}

// escapePathValue percent-encodes an atomic path value. Only RFC 3986
// unreserved bytes stay raw; allowDot=false additionally encodes '.' so a
// dot in the data cannot become a label-explode delimiter.
func escapePathValue(s string, allowDot bool) string {
	var b strings.Builder
	b.Grow(len(s))
	appendPercentEncoded(&b, s, func(c byte) bool {
		return isUnreservedByte(c) && (allowDot || c != '.')
	}, false)
	return b.String()
}

// isReservedByte reports an RFC 3986 reserved character allowed raw only by
// allowReserved.
func isReservedByte(b byte) bool {
	switch b {
	case ':', '/', '?', '#', '[', ']', '@', '!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=':
		return true
	}
	return false
}

// escapeQueryName percent-encodes an atomic query key. Keys are always
// encoded strictly: allowReserved applies to values only, so delimiters in a
// key can never break the query structure.
func escapeQueryName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	appendPercentEncoded(&b, s, isUnreservedByte, true)
	return b.String()
}

// escapeQueryValue percent-encodes an atomic query value. With allowReserved,
// RFC 3986 reserved characters stay raw as permitted by the parameter.
func escapeQueryValue(s string, allowReserved bool) string {
	var b strings.Builder
	b.Grow(len(s))
	appendPercentEncoded(&b, s, func(c byte) bool {
		return isUnreservedByte(c) || (allowReserved && isReservedByte(c))
	}, true)
	return b.String()
}

// ---- path ---------------------------------------------------------------

func validPathMatrixName(name string) bool {
	// The name is emitted verbatim in ";name=" for matrix style, so it may
	// only use unreserved bytes.
	for i := 0; i < len(name); i++ {
		if !isUnreservedByte(name[i]) {
			return false
		}
	}
	return name != ""
}

func encodePathFragment(param *openapi3.Parameter, sm *openapi3.SerializationMethod, value any) (string, error) {
	shape, ok := shapeOf(value)
	if !ok {
		return "", newEncodingError(EncodingErrorUnsupported, param, sm, value,
			fmt.Sprintf("values of type %T are not supported", value), nil)
	}
	if value == nil {
		return "", newEncodingError(EncodingErrorUnsupported, param, sm, value,
			"null has no serialization representation in the parameter styles", nil)
	}

	switch sm.Style {
	case openapi3.SerializationSimple, openapi3.SerializationLabel, openapi3.SerializationMatrix:
	default:
		return "", newEncodingError(EncodingErrorUnsupported, param, sm, value,
			fmt.Sprintf("style %q is not defined for path parameters", sm.Style), nil)
	}
	if sm.Style == openapi3.SerializationMatrix && !validPathMatrixName(param.Name) {
		return "", newEncodingError(EncodingErrorUnsupported, param, sm, value,
			fmt.Sprintf("matrix parameter name %q contains characters that cannot be emitted verbatim", param.Name), nil)
	}

	label := sm.Style == openapi3.SerializationLabel
	matrix := sm.Style == openapi3.SerializationMatrix
	// A dot is a delimiter only for label-explode arrays/objects; everywhere
	// else a literal dot is unambiguous and stays raw (it is unreserved).
	dotIsStructural := label && sm.Explode && shape != shapeScalar

	esc := func(s string) string { return escapePathValue(s, !dotIsStructural) }

	switch shape {
	case shapeScalar:
		s, err := scalarString(value, nil)
		if err != nil {
			return "", attachParam(param, sm, err)
		}
		body := esc(s)
		switch {
		case matrix:
			return ";" + param.Name + "=" + body, nil
		case label:
			return "." + body, nil
		default:
			return body, nil
		}
	case shapeArray:
		parts, err := arrayScalarStrings(value.([]any))
		if err != nil {
			return "", attachParam(param, sm, err)
		}
		for i, p := range parts {
			parts[i] = esc(p)
		}
		switch {
		case matrix && sm.Explode:
			return ";" + param.Name + "=" + strings.Join(parts, ";"+param.Name+"="), nil
		case matrix:
			return ";" + param.Name + "=" + strings.Join(parts, ","), nil
		case label && sm.Explode:
			return "." + strings.Join(parts, "."), nil
		case label:
			// "." + join yields "." for an empty array too.
			return "." + strings.Join(parts, ","), nil
		default:
			return strings.Join(parts, ","), nil
		}
	case shapeObject:
		entries, err := objectScalarEntries(value.(map[string]any))
		if err != nil {
			return "", attachParam(param, sm, err)
		}
		switch {
		case sm.Explode:
			pairs := make([]string, len(entries))
			for i, e := range entries {
				pairs[i] = esc(e.name) + "=" + esc(e.value)
			}
			switch {
			case matrix:
				if len(entries) == 0 {
					return ";", nil
				}
				return ";" + strings.Join(pairs, ";"), nil
			case label:
				if len(entries) == 0 {
					return ".", nil
				}
				return "." + strings.Join(pairs, "."), nil
			default:
				return strings.Join(pairs, ","), nil
			}
		default:
			flat := make([]string, 0, len(entries)*2)
			for _, e := range entries {
				flat = append(flat, esc(e.name), esc(e.value))
			}
			switch {
			case matrix:
				if len(entries) == 0 {
					return ";" + param.Name + "=", nil
				}
				return ";" + param.Name + "=" + strings.Join(flat, ","), nil
			case label:
				if len(entries) == 0 {
					return ".", nil
				}
				return "." + strings.Join(flat, ","), nil
			default:
				return strings.Join(flat, ","), nil
			}
		}
	}
	panic("unreachable")
}

// ---- query --------------------------------------------------------------

func encodeQueryPairs(param *openapi3.Parameter, sm *openapi3.SerializationMethod, value any) ([]EncodedQueryPair, error) {
	if param.Name == "" {
		return nil, newEncodingError(EncodingErrorUnsupported, param, sm, value, "query parameter name is empty", nil)
	}
	shape, ok := shapeOf(value)
	if !ok {
		return nil, newEncodingError(EncodingErrorUnsupported, param, sm, value,
			fmt.Sprintf("values of type %T are not supported", value), nil)
	}
	if value == nil {
		return nil, newEncodingError(EncodingErrorUnsupported, param, sm, value,
			"null has no serialization representation in the parameter styles", nil)
	}

	name := escapeQueryName(param.Name)
	allowReserved := param.AllowReserved
	val := func(s string) string { return escapeQueryValue(s, allowReserved) }

	switch sm.Style {
	case openapi3.SerializationForm:
		// form is defined for primitives, arrays and objects.
	case openapi3.SerializationSpaceDelimited, openapi3.SerializationPipeDelimited:
		if shape != shapeArray {
			return nil, newEncodingError(EncodingErrorUnsupported, param, sm, value,
				fmt.Sprintf("style %q is only defined for arrays", sm.Style), nil)
		}
	case openapi3.SerializationDeepObject:
		if shape != shapeObject || !sm.Explode {
			return nil, newEncodingError(EncodingErrorUnsupported, param, sm, value,
				"style \"deepObject\" is only defined for objects with explode=true", nil)
		}
	default:
		return nil, newEncodingError(EncodingErrorUnsupported, param, sm, value,
			fmt.Sprintf("style %q is not defined for query parameters", sm.Style), nil)
	}

	switch shape {
	case shapeScalar:
		if sm.Style != openapi3.SerializationForm {
			return nil, newEncodingError(EncodingErrorUnsupported, param, sm, value,
				fmt.Sprintf("style %q is only defined for arrays", sm.Style), nil)
		}
		s, err := scalarString(value, nil)
		if err != nil {
			return nil, attachParam(param, sm, err)
		}
		return []EncodedQueryPair{{Key: name, Value: val(s)}}, nil

	case shapeArray:
		items, err := arrayScalarStrings(value.([]any))
		if err != nil {
			return nil, attachParam(param, sm, err)
		}
		if sm.Explode {
			// One pair per item, in order; repeated keys are preserved.
			pairs := make([]EncodedQueryPair, 0, len(items))
			for _, item := range items {
				pairs = append(pairs, EncodedQueryPair{Key: name, Value: val(item)})
			}
			// An exploded empty container has no defined wire token.
			return pairs, nil
		}
		parts := make([]string, len(items))
		for i, item := range items {
			parts[i] = val(item)
		}
		var delim string
		switch sm.Style {
		case openapi3.SerializationSpaceDelimited:
			// The space delimiter is percent-encoded on the wire.
			delim = "%20"
		case openapi3.SerializationPipeDelimited:
			delim = "|"
		default:
			delim = ","
		}
		return []EncodedQueryPair{{Key: name, Value: strings.Join(parts, delim)}}, nil

	case shapeObject:
		entries, err := objectScalarEntries(value.(map[string]any))
		if err != nil {
			return nil, attachParam(param, sm, err)
		}
		switch sm.Style {
		case openapi3.SerializationDeepObject:
			pairs := make([]EncodedQueryPair, 0, len(entries))
			for _, e := range entries {
				key := name + "[" + escapeQueryName(e.name) + "]"
				pairs = append(pairs, EncodedQueryPair{Key: key, Value: val(e.value)})
			}
			return pairs, nil
		case openapi3.SerializationForm:
			if sm.Explode {
				pairs := make([]EncodedQueryPair, 0, len(entries))
				for _, e := range entries {
					pairs = append(pairs, EncodedQueryPair{Key: escapeQueryName(e.name), Value: val(e.value)})
				}
				return pairs, nil
			}
			flat := make([]string, 0, len(entries)*2)
			for _, e := range entries {
				flat = append(flat, escapeQueryName(e.name), val(e.value))
			}
			return []EncodedQueryPair{{Key: name, Value: strings.Join(flat, ",")}}, nil
		default:
			return nil, newEncodingError(EncodingErrorUnsupported, param, sm, value,
				fmt.Sprintf("style %q is not defined for objects", sm.Style), nil)
		}
	}
	panic("unreachable")
}

// ---- header -------------------------------------------------------------

// isLegalHeaderFieldByte implements RFC 9110 field-value contents: HTAB,
// SP through '~' except DEL, and obs-text (0x80+). CR/LF/NUL and other
// control bytes are rejected.
func isLegalHeaderFieldByte(b byte) bool {
	return b == '\t' || (b >= 0x20 && b != 0x7f && b <= 0x7e) || b >= 0x80
}

// isLegalHeaderTokenByte implements RFC 9110 tchar for field names.
func isLegalHeaderTokenByte(b byte) bool {
	if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') {
		return true
	}
	switch b {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}

func checkHeaderText(param *openapi3.Parameter, sm *openapi3.SerializationMethod, value any, s, what string, forbidden string) error {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isLegalHeaderFieldByte(c) {
			return newEncodingError(EncodingErrorIllegal, param, sm, value,
				fmt.Sprintf("%s contains byte %#02x that is illegal in an HTTP header field", what, c), nil)
		}
		if strings.IndexByte(forbidden, c) >= 0 {
			return newEncodingError(EncodingErrorUnsupported, param, sm, value,
				fmt.Sprintf("%s contains %q, which is a style delimiter and cannot be escaped in header simple style", what, string(c)), nil)
		}
	}
	return nil
}

func encodeHeaderValue(param *openapi3.Parameter, sm *openapi3.SerializationMethod, value any) (string, error) {
	if sm.Style != openapi3.SerializationSimple {
		return "", newEncodingError(EncodingErrorUnsupported, param, sm, value,
			fmt.Sprintf("style %q is not defined for header parameters", sm.Style), nil)
	}
	if param.Name == "" {
		return "", newEncodingError(EncodingErrorUnsupported, param, sm, value, "header parameter name is empty", nil)
	}
	for i := 0; i < len(param.Name); i++ {
		if !isLegalHeaderTokenByte(param.Name[i]) {
			return "", newEncodingError(EncodingErrorUnsupported, param, sm, value,
				fmt.Sprintf("header name %q is not a valid HTTP token", param.Name), nil)
		}
	}

	shape, ok := shapeOf(value)
	if !ok {
		return "", newEncodingError(EncodingErrorUnsupported, param, sm, value,
			fmt.Sprintf("values of type %T are not supported", value), nil)
	}
	if value == nil {
		return "", newEncodingError(EncodingErrorUnsupported, param, sm, value,
			"null has no serialization representation in the parameter styles", nil)
	}

	switch shape {
	case shapeScalar:
		s, err := scalarString(value, nil)
		if err != nil {
			return "", attachParam(param, sm, err)
		}
		if err := checkHeaderText(param, sm, value, s, "value", ""); err != nil {
			return "", err
		}
		return s, nil
	case shapeArray:
		items, err := arrayScalarStrings(value.([]any))
		if err != nil {
			return "", attachParam(param, sm, err)
		}
		for i, item := range items {
			if err := checkHeaderText(param, sm, value, item, fmt.Sprintf("item %d", i), ","); err != nil {
				return "", err
			}
		}
		return strings.Join(items, ","), nil
	case shapeObject:
		entries, err := objectScalarEntries(value.(map[string]any))
		if err != nil {
			return "", attachParam(param, sm, err)
		}
		// Keys and values may never contain the comma pair delimiter; in
		// exploded form the '=' between key and value is structural too.
		forbidden := ","
		if sm.Explode {
			forbidden = ",="
		}
		for _, e := range entries {
			if err := checkHeaderText(param, sm, value, e.name, "property name", forbidden); err != nil {
				return "", err
			}
			if err := checkHeaderText(param, sm, value, e.value, "property value", forbidden); err != nil {
				return "", err
			}
		}
		if sm.Explode {
			pairs := make([]string, len(entries))
			for i, e := range entries {
				pairs[i] = e.name + "=" + e.value
			}
			return strings.Join(pairs, ","), nil
		}
		flat := make([]string, 0, len(entries)*2)
		for _, e := range entries {
			flat = append(flat, e.name, e.value)
		}
		return strings.Join(flat, ","), nil
	}
	panic("unreachable")
}

// ---- cookie -------------------------------------------------------------

// isCookieOctetByte implements RFC 6265 cookie-octet:
// %x21 / %x23-2B / %x2D-3A / %x3C-5B / %x5D-7E. In particular it excludes
// spaces, '"', ',', ';' and '\'.
func isCookieOctetByte(b byte) bool {
	return b == 0x21 ||
		(b >= 0x23 && b <= 0x2b) ||
		(b >= 0x2d && b <= 0x3a) ||
		(b >= 0x3c && b <= 0x5b) ||
		(b >= 0x5d && b <= 0x7e)
}

// isCookieNameByte is cookie-octet without '=' (and without the separators
// already excluded by cookie-octet).
func isCookieNameByte(b byte) bool {
	return isCookieOctetByte(b) && b != '='
}

func checkCookieText(param *openapi3.Parameter, sm *openapi3.SerializationMethod, value any, s, what string) error {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isCookieOctetByte(c) {
			switch {
			case c == '\r' || c == '\n':
				return newEncodingError(EncodingErrorIllegal, param, sm, value,
					fmt.Sprintf("%s contains a carriage return or line feed, which is illegal in a cookie", what), nil)
			case c == ',' || c == ';':
				return newEncodingError(EncodingErrorUnsupported, param, sm, value,
					fmt.Sprintf("%s contains %q, which is a cookie/form style delimiter and cannot be escaped", what, string(c)), nil)
			default:
				return newEncodingError(EncodingErrorIllegal, param, sm, value,
					fmt.Sprintf("%s contains byte %#02x that is not a legal cookie-octet (RFC 6265)", what, c), nil)
			}
		}
	}
	return nil
}

func encodeCookieValue(param *openapi3.Parameter, sm *openapi3.SerializationMethod, value any) (string, error) {
	if sm.Style != openapi3.SerializationForm {
		return "", newEncodingError(EncodingErrorUnsupported, param, sm, value,
			fmt.Sprintf("style %q is not defined for cookie parameters", sm.Style), nil)
	}
	if param.Name == "" {
		return "", newEncodingError(EncodingErrorUnsupported, param, sm, value, "cookie parameter name is empty", nil)
	}
	for i := 0; i < len(param.Name); i++ {
		if !isCookieNameByte(param.Name[i]) {
			return "", newEncodingError(EncodingErrorIllegal, param, sm, value,
				fmt.Sprintf("cookie name %q contains byte %#02x that is not a legal cookie name", param.Name, param.Name[i]), nil)
		}
	}

	shape, ok := shapeOf(value)
	if !ok {
		return "", newEncodingError(EncodingErrorUnsupported, param, sm, value,
			fmt.Sprintf("values of type %T are not supported", value), nil)
	}
	if value == nil {
		return "", newEncodingError(EncodingErrorUnsupported, param, sm, value,
			"null has no serialization representation in the parameter styles", nil)
	}

	switch shape {
	case shapeScalar:
		s, err := scalarString(value, nil)
		if err != nil {
			return "", attachParam(param, sm, err)
		}
		if err := checkCookieText(param, sm, value, s, "value"); err != nil {
			return "", err
		}
		return s, nil
	case shapeArray:
		// Cookies only define the non-exploded comma form, which is also the
		// only array form the decoder accepts.
		if sm.Explode {
			return "", newEncodingError(EncodingErrorUnsupported, param, sm, value,
				"exploded arrays are not defined for cookie parameters", nil)
		}
		items, err := arrayScalarStrings(value.([]any))
		if err != nil {
			return "", attachParam(param, sm, err)
		}
		for i, item := range items {
			if err := checkCookieText(param, sm, value, item, fmt.Sprintf("item %d", i)); err != nil {
				return "", err
			}
		}
		return strings.Join(items, ","), nil
	case shapeObject:
		if sm.Explode {
			return "", newEncodingError(EncodingErrorUnsupported, param, sm, value,
				"exploded objects are not defined for cookie parameters", nil)
		}
		entries, err := objectScalarEntries(value.(map[string]any))
		if err != nil {
			return "", attachParam(param, sm, err)
		}
		flat := make([]string, 0, len(entries)*2)
		for _, e := range entries {
			if err := checkCookieText(param, sm, value, e.name, "property name"); err != nil {
				return "", err
			}
			if err := checkCookieText(param, sm, value, e.value, "property value"); err != nil {
				return "", err
			}
			flat = append(flat, e.name, e.value)
		}
		return strings.Join(flat, ","), nil
	}
	panic("unreachable")
}

// attachParam fills parameter context into errors produced while walking a
// value (scalarString reports without it).
func attachParam(param *openapi3.Parameter, sm *openapi3.SerializationMethod, err error) error {
	var ee *EncodingError
	if errors.As(err, &ee) {
		if ee.Parameter == nil {
			ee.Parameter = param
			ee.Style, ee.Explode = sm.Style, sm.Explode
		}
		return ee
	}
	return err
}
