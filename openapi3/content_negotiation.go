package openapi3

import (
	"errors"
	"fmt"
	"strings"
)

// This file implements server-driven response content negotiation
// (RFC 9110, section 12.5.1) for a response's Content map.
//
// An OpenAPI document describes responses with media *ranges* (e.g.
// "application/*" or "*/*"); it never enumerates every concrete type a server
// might produce. The caller therefore passes the ordered list of concrete
// media types the server is actually able to serve (offers), which turns the
// document's ranges into concrete output types. A concrete type that no
// Content entry covers is a server-side programming error and is reported
// before negotiation runs: a wildcard entry must never be used to invent a
// representation out of thin air.

// DefaultAcceptQuality is the quality ("q") of an Accept media range that
// carries no q parameter (RFC 9110, section 12.5.1).
const DefaultAcceptQuality AcceptQuality = 1000

// ErrNotAcceptable is matched (via errors.Is) by a NotAcceptableError: the
// request named only media types the server cannot provide, or assigned
// quality 0 to every offered type.
var ErrNotAcceptable = errors.New("no acceptable response media type")

// AcceptQuality is an Accept media range's relative quality factor ("q",
// RFC 9110 section 12.5.1) stored as an integer number of thousandths in the
// range 0..1000. Storing it as an integer keeps ordering exact: values such
// as 0.001 never go through a floating-point approximation.
type AcceptQuality uint16

// Thousandths returns q as an integer in the range 0..1000. Compare qualities
// with this value rather than converting to a float.
func (q AcceptQuality) Thousandths() int { return int(q) }

// String renders q the way it is written in an Accept header: 1000 as "1",
// 500 as "0.5", 1 as "0.001", 0 as "0".
func (q AcceptQuality) String() string {
	switch {
	case q >= 1000:
		return "1"
	case q <= 0:
		return "0"
	default:
		s := fmt.Sprintf("%03d", q)
		return "0." + strings.TrimRight(s, "0")
	}
}

// AcceptMediaType is one media range of a parsed Accept header.
type AcceptMediaType struct {
	// Raw is the entry verbatim, with surrounding whitespace trimmed
	// (e.g. `application/json; q=0.5`).
	Raw string
	// Type and Subtype are the lowercased media range parts. "*" denotes a
	// wildcard; when Type is "*" Subtype is also "*".
	Type    string
	Subtype string
	// Params holds the media type parameters other than "q", keyed by their
	// lowercased name. Parameter values keep their original case: media type
	// names are case-insensitive, parameter values are not.
	Params map[string]string
	// Quality is the entry's q value, DefaultAcceptQuality when absent.
	Quality AcceptQuality
	// Header is the index of the Accept header this entry came from among
	// the headers passed to ParseAccept, and Index its position in the
	// merged list of entries (header order first, then within the header).
	Header, Index int
}

// MediaRange returns the lowercased "type/subtype" of the entry without
// parameters.
func (m AcceptMediaType) MediaRange() string { return m.Type + "/" + m.Subtype }

// ParseAccept parses all of a request's Accept header fields into one ordered
// list of media ranges. Multiple header fields are merged in the order given,
// as HTTP combines same-named fields as a comma-separated list. Commas and
// semicolons inside quoted parameter values do not separate entries or
// parameters.
//
// With no headers (or only empty/whitespace header values), ParseAccept
// returns (nil, nil): per RFC 9110 an absent Accept header accepts anything,
// which is distinct from an "Accept: */*" entry.
//
// A malformed header yields *AcceptParseError; it is never silently replaced
// with */*.
func ParseAccept(headers ...string) ([]AcceptMediaType, error) {
	entries := make([]AcceptMediaType, 0)
	for hi, header := range headers {
		parts, err := splitHeaderList(header)
		if err != nil {
			return nil, &AcceptParseError{Header: hi, Value: header, Reason: err.Error()}
		}
		for _, part := range parts {
			entry := AcceptMediaType{
				Raw:     part,
				Quality: DefaultAcceptQuality,
				Header:  hi,
				Index:   len(entries),
			}
			if err := parseAcceptEntry(part, &entry); err != nil {
				return nil, &AcceptParseError{Header: hi, Value: header, Entry: part, Reason: err.Error()}
			}
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}
	return entries, nil
}

// ResponseContentChoice is the result of Content.NegotiateResponse.
type ResponseContentChoice struct {
	// ContentType is the concrete media type to send, copied verbatim from
	// the offers list (parameters and their case preserved).
	ContentType string
	// MediaType is the Content entry that documents ContentType. It is the
	// very pointer stored in the Content map, so its identity is preserved.
	MediaType *MediaType
	// ContentKey is the Content map key that MediaType was found under.
	ContentKey string
	// Quality is the effective quality associated with the choice, taken
	// from AcceptEntry. It is DefaultAcceptQuality when no Accept header was
	// present.
	Quality AcceptQuality
	// AcceptEntry points at the Accept media range that decided the match.
	// It is nil when no Accept header was present.
	AcceptEntry *AcceptMediaType
}

// NegotiateResponse selects the concrete response media type to produce.
//
// offers lists the concrete media types the server can serve, in the server's
// order of preference (most preferred first); every offer must be covered by
// content (exactly, via "type/*", or via "*/*"), and an offer not covered by
// the document is reported with *OfferNotDocumentedError before any request
// data is looked at. acceptHeaders are all of the request's Accept header
// fields, in order; pass none when the request has no Accept header.
//
// Each offer's effective quality comes from the most specific matching Accept
// range: an exact type/subtype with more matching parameters beats
// "type/*", which beats "*/*". So "application/json;q=0, */*;q=1" excludes
// JSON even though the wildcard has a higher q. Among ranges of equal
// specificity the highest q wins; the structured-syntax suffix ("+json") is
// not treated as a wildcard and "*+json" is not supported.
//
// The offer with the highest effective quality wins. Ties are resolved
// deterministically by match specificity, then Accept entry order, then the
// order of offers — never by Go map iteration order. When every offer has
// quality 0 or none is acceptable, the error matches ErrNotAcceptable. A
// malformed Accept header yields *AcceptParseError.
//
// Neither content nor the header strings are modified.
func (content Content) NegotiateResponse(offers []string, acceptHeaders ...string) (*ResponseContentChoice, error) {
	if len(offers) == 0 {
		return nil, &NotAcceptableError{Reason: "no concrete response media types were offered"}
	}

	parsed := make([]parsedMediaType, len(offers))
	for i, offer := range offers {
		pm, err := parseConcreteMediaType(offer)
		if err != nil {
			return nil, &InvalidOfferMediaTypeError{Offer: offer, Reason: err.Error()}
		}
		// Coverage is a property of the document and the server, not of the
		// request: check it before consulting Accept.
		if _, mt := content.resolve(pm); mt == nil {
			return nil, &OfferNotDocumentedError{Offer: offer}
		}
		parsed[i] = pm
	}

	accept, err := ParseAccept(acceptHeaders...)
	if err != nil {
		return nil, err
	}
	if len(accept) == 0 {
		// No Accept header: every concrete type is acceptable; the server's
		// first offer expresses its preference.
		key, mt := content.resolve(parsed[0])
		return &ResponseContentChoice{
			ContentType: offers[0],
			MediaType:   mt,
			ContentKey:  key,
			Quality:     DefaultAcceptQuality,
			AcceptEntry: nil,
		}, nil
	}

	var best *negotiationCandidate
	for i, offer := range parsed {
		// Among the ranges matching this offer keep the most specific, and
		// among ranges of equal specificity the one with the highest q.
		var specificity int = -1 // exactness*1e6 + matched parameter count
		var quality AcceptQuality
		entry := -1
		found := false
		for j := range accept {
			exactness, paramCount, ok := matchScore(accept[j], offer)
			if !ok {
				continue
			}
			score := exactness*1_000_000 + paramCount
			switch {
			case !found || score > specificity:
				found = true
				specificity = score
				quality = accept[j].Quality
				entry = j
			case score == specificity && accept[j].Quality > quality:
				quality = accept[j].Quality
				entry = j
			}
		}
		if !found || quality == 0 {
			continue
		}
		c := negotiationCandidate{
			offerIndex:   i,
			entryIndex:   entry,
			quality:      quality,
			exactness:    specificity / 1_000_000,
			matchedParam: specificity % 1_000_000,
		}
		switch {
		case best == nil:
			best = &c
		case betterCandidate(c, *best):
			best = &c
		}
	}
	if best == nil {
		return nil, &NotAcceptableError{Offers: offers, Accept: accept}
	}

	key, mt := content.resolve(parsed[best.offerIndex])
	entry := accept[best.entryIndex]
	return &ResponseContentChoice{
		ContentType: offers[best.offerIndex],
		MediaType:   mt,
		ContentKey:  key,
		Quality:     best.quality,
		AcceptEntry: &entry,
	}, nil
}

// negotiationCandidate is one acceptable offer together with the Accept
// range that makes it acceptable and its match specificity.
type negotiationCandidate struct {
	offerIndex   int
	entryIndex   int
	quality      AcceptQuality
	exactness    int
	matchedParam int
}

// betterCandidate reports whether c wins over the current best: highest
// quality first, then match specificity, then earlier Accept entry, then
// earlier offer.
func betterCandidate(c, best negotiationCandidate) bool {
	if c.quality != best.quality {
		return c.quality > best.quality
	}
	if c.exactness != best.exactness {
		return c.exactness > best.exactness
	}
	if c.matchedParam != best.matchedParam {
		return c.matchedParam > best.matchedParam
	}
	if c.entryIndex != best.entryIndex {
		return c.entryIndex < best.entryIndex
	}
	return c.offerIndex < best.offerIndex
}

// matchScore reports how specifically e matches the concrete offer:
// exactness 2 for type/subtype, 1 for "type/*", 0 for "*/*", together with
// the number of the range's parameters that match (all must match for the
// range to match at all).
func matchScore(e AcceptMediaType, offer parsedMediaType) (exactness int, matchedParams int, ok bool) {
	switch {
	case e.Type == "*":
		exactness = 0
	case e.Type != offer.typ:
		return 0, 0, false
	case e.Subtype == "*":
		exactness = 1
	case e.Subtype != offer.subtype:
		return 0, 0, false
	default:
		exactness = 2
	}
	for name, value := range e.Params {
		got, present := offer.params[name]
		if !present || got != value {
			return 0, 0, false
		}
	}
	return exactness, len(e.Params), true
}

// AcceptParseError reports a malformed Accept header field. It is never
// treated as "*\u002f*": a request that fails to send a syntactically valid
// Accept header must not silently get a representation chosen for it.
type AcceptParseError struct {
	// Header is the index of the offending header among the headers parsed.
	Header int
	// Value is the raw offending header value.
	Value string
	// Entry is the offending comma-separated entry, when isolated.
	Entry string
	// Reason describes the syntax violation.
	Reason string
}

func (e *AcceptParseError) Error() string {
	if e.Entry != "" {
		return fmt.Sprintf("malformed Accept header %d: %q: %s", e.Header, e.Entry, e.Reason)
	}
	return fmt.Sprintf("malformed Accept header %d: %s", e.Header, e.Reason)
}

// InvalidOfferMediaTypeError reports that an offered media type is not a
// concrete media type (e.g. it is a wildcard or syntactically invalid).
type InvalidOfferMediaTypeError struct {
	// Offer is the offending offered value.
	Offer string
	// Reason describes why it is not a concrete media type.
	Reason string
}

func (e *InvalidOfferMediaTypeError) Error() string {
	return fmt.Sprintf("offered media type %q is not a concrete media type: %s", e.Offer, e.Reason)
}

// OfferNotDocumentedError reports that the server offers a concrete media
// type for which the response Content describes no schema: not exactly, not
// via a "type/*" range, and not via "*\u002f*". Such an offer must be fixed
// in the document or removed from the offers; negotiation must not invent a
// schema for it from a wildcard.
type OfferNotDocumentedError struct {
	// Offer is the undocumented concrete media type.
	Offer string
}

func (e *OfferNotDocumentedError) Error() string {
	return fmt.Sprintf("offered media type %q is not described by the response Content", e.Offer)
}

// NotAcceptableError reports that negotiation found no representation the
// request accepts: the Accept ranges cover none of the offers, or every
// covered offer has effective quality 0. It matches ErrNotAcceptable.
type NotAcceptableError struct {
	// Offers lists the concrete media types the server could produce, in
	// preference order.
	Offers []string
	// Accept lists the parsed Accept ranges, when any were present.
	Accept []AcceptMediaType
	// Reason is set for negotiation inputs that cannot be satisfied without
	// any candidate at all (e.g. no offers).
	Reason string
}

func (e *NotAcceptableError) Error() string {
	if e.Reason != "" {
		return e.Reason
	}
	return fmt.Sprintf("%s for offers %q", ErrNotAcceptable, e.Offers)
}

// Is lets errors.Is(err, ErrNotAcceptable) match.
func (e *NotAcceptableError) Is(target error) bool { return target == ErrNotAcceptable }

// parsedMediaType is a parsed concrete media type offered by the server.
type parsedMediaType struct {
	raw     string
	typ     string // lowercased
	subtype string // lowercased
	params  map[string]string
}

// parseConcreteMediaType parses an offer: a type/subtype with no wildcards,
// optionally followed by parameters.
func parseConcreteMediaType(s string) (parsedMediaType, error) {
	var pm parsedMediaType
	segments, err := splitParameters(s)
	if err != nil {
		return pm, err
	}
	typ, subtype, err := parseMediaRange(strings.TrimSpace(segments[0]))
	if err != nil {
		return pm, err
	}
	if typ == "*" || subtype == "*" {
		return pm, fmt.Errorf("wildcard ranges (%s) are not concrete", typ+"/"+subtype)
	}
	params, err := parseParams(segments[1:], true)
	if err != nil {
		return pm, err
	}
	return parsedMediaType{raw: s, typ: typ, subtype: subtype, params: params}, nil
}

// parseAcceptEntry fills e from one comma-separated Accept entry.
func parseAcceptEntry(s string, e *AcceptMediaType) error {
	segments, err := splitParameters(s)
	if err != nil {
		return err
	}
	typ, subtype, err := parseMediaRange(strings.TrimSpace(segments[0]))
	if err != nil {
		return err
	}
	params, err := parseParams(segments[1:], false)
	if err != nil {
		return err
	}
	if q, hasQ := params["q"]; hasQ {
		quality, err := parseQValue(q)
		if err != nil {
			return err
		}
		e.Quality = quality
		delete(params, "q")
	}
	e.Type = typ
	e.Subtype = subtype
	e.Params = params
	return nil
}

// parseMediaRange splits and validates a "type/subtype" token. The returned
// parts are lowercased. A "*" type requires a "*" subtype.
func parseMediaRange(s string) (typ, subtype string, err error) {
	if s == "" {
		return "", "", errors.New("empty media type")
	}
	slash := strings.IndexByte(s, '/')
	if slash < 0 {
		return "", "", fmt.Errorf("media type %q is missing a slash", s)
	}
	typ = s[:slash]
	subtype = s[slash+1:]
	if strings.ContainsRune(subtype, '/') {
		return "", "", fmt.Errorf("media type %q has too many slashes", s)
	}
	if !isToken(typ) {
		return "", "", fmt.Errorf("invalid media type %q: %q is not a token", s, typ)
	}
	if subtype != "*" && !isToken(subtype) {
		return "", "", fmt.Errorf("invalid media type %q: %q is not a token", s, subtype)
	}
	typ = strings.ToLower(typ)
	if typ == "*" && subtype != "*" {
		return "", "", fmt.Errorf("invalid media type %q: wildcard type requires wildcard subtype", s)
	}
	subtype = strings.ToLower(subtype)
	return typ, subtype, nil
}

// parseParams parses ";name=value" segments. Parameter names are
// case-insensitive (returned lowercased); values keep their case and may be
// quoted. For offers (isOffer true) every parameter is media type metadata.
// For Accept entries a "q" parameter is extracted as the quality and at most
// one is allowed.
func parseParams(segments []string, isOffer bool) (map[string]string, error) {
	var params map[string]string
	for _, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			return nil, errors.New("empty media type parameter")
		}
		eq := strings.IndexByte(segment, '=')
		if eq < 0 {
			return nil, fmt.Errorf("parameter %q is missing a value", segment)
		}
		name := strings.ToLower(strings.TrimSpace(segment[:eq]))
		raw := strings.TrimSpace(segment[eq+1:])
		if name == "" || !isToken(name) {
			return nil, fmt.Errorf("invalid parameter name %q", segment[:eq])
		}
		value, err := parseParamValue(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid value for parameter %q: %w", name, err)
		}
		if params == nil {
			params = make(map[string]string)
		}
		if _, dup := params[name]; dup {
			return nil, fmt.Errorf("parameter %q is set more than once", name)
		}
		params[name] = value
	}
	return params, nil
}

// parseParamValue reads either a token or a quoted-string (RFC 9110).
func parseParamValue(s string) (string, error) {
	if s == "" {
		return "", errors.New("empty value")
	}
	if s[0] != '"' {
		if !isToken(s) {
			return "", fmt.Errorf("%q is not a token", s)
		}
		return s, nil
	}
	var b strings.Builder
	i := 1
	for i < len(s) {
		c := s[i]
		switch {
		case c == '"':
			if i != len(s)-1 {
				return "", errors.New("characters after closing quote")
			}
			return b.String(), nil
		case c == '\\' && i+1 < len(s):
			next := s[i+1]
			if next == '\t' || next >= 0x20 {
				b.WriteByte(next)
				i += 2
				continue
			}
			return "", errors.New("bad quoted-pair escape")
		case c == '\t' || c >= 0x20:
			b.WriteByte(c)
			i++
		default:
			return "", errors.New("control character in quoted-string")
		}
	}
	return "", errors.New("unterminated quoted-string")
}

// splitHeaderList splits one Accept header field into its comma-separated
// entries, ignoring commas inside quoted strings. An all-whitespace header
// yields no entries.
func splitHeaderList(header string) ([]string, error) {
	var out []string
	start := 0
	inQuote := false
	sawComma := false
	for i := 0; i < len(header); i++ {
		c := header[i]
		if inQuote {
			switch c {
			case '\\':
				i++ // the escaped byte cannot end the quote
			case '"':
				inQuote = false
			}
			continue
		}
		switch c {
		case '"':
			inQuote = true
		case ',':
			sawComma = true
			part := strings.TrimSpace(header[start:i])
			if part == "" {
				return nil, fmt.Errorf("empty media type in %q", header)
			}
			out = append(out, part)
			start = i + 1
		}
	}
	if inQuote {
		return nil, errors.New("unterminated quoted-string")
	}
	last := strings.TrimSpace(header[start:])
	switch {
	case last != "":
		out = append(out, last)
	case sawComma:
		return nil, fmt.Errorf("empty media type in %q", header)
	}
	return out, nil
}

// splitParameters splits one media type entry on semicolons, ignoring
// semicolons inside quoted strings, returning the media type token first.
func splitParameters(entry string) ([]string, error) {
	var out []string
	start := 0
	inQuote := false
	for i := 0; i < len(entry); i++ {
		c := entry[i]
		if inQuote {
			switch c {
			case '\\':
				i++
			case '"':
				inQuote = false
			}
			continue
		}
		switch c {
		case '"':
			inQuote = true
		case ';':
			out = append(out, entry[start:i])
			start = i + 1
		}
	}
	if inQuote {
		return nil, errors.New("unterminated quoted-string")
	}
	out = append(out, entry[start:])
	return out, nil
}

// parseQValue validates an Accept q value per RFC 9110's grammar:
//
//	qvalue = ( "0" [ "." 0*3DIGIT ] ) / ( "1" [ "." 0*3("0") ] )
//
// and returns it as thousandths without ever parsing a float.
func parseQValue(s string) (AcceptQuality, error) {
	if s == "" {
		return 0, errors.New("q requires a value")
	}
	intPart := s
	frac := ""
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		intPart = s[:dot]
		frac = s[dot+1:]
	}
	switch intPart {
	case "0":
		if len(frac) > 3 {
			return 0, fmt.Errorf("q %q has more than 3 decimal digits", s)
		}
	case "1":
		if len(frac) > 3 {
			return 0, fmt.Errorf("q %q has more than 3 decimal digits", s)
		}
		for _, c := range frac {
			if c != '0' {
				return 0, fmt.Errorf("q %q is greater than 1", s)
			}
		}
		return DefaultAcceptQuality, nil
	default:
		return 0, fmt.Errorf("q %q must be between 0 and 1", s)
	}
	for _, c := range frac {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("q %q is not a decimal number", s)
		}
	}
	if frac == "" {
		return 0, nil
	}
	v := int(frac[0] - '0')
	for i := 1; i < 3; i++ {
		v *= 10
		if i < len(frac) {
			v += int(frac[i] - '0')
		}
	}
	return AcceptQuality(v), nil
}

// isToken reports whether s is an RFC 9110 token (1*tchar).
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isTokenRune(r) {
			return false
		}
	}
	return true
}

func isTokenRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", r)
}

// resolve finds the Content key and MediaType that describe the concrete
// offer, following the same specificity order as Content.Get but without
// relying on map iteration order: full match (parameters included), then the
// parameterless type/subtype, then "type/*", then "*/*".
func (content Content) resolve(offer parsedMediaType) (string, *MediaType) {
	full := strings.ToLower(strings.TrimSpace(offer.raw))
	base := offer.typ + "/" + offer.subtype
	candidates := []string{full, base, offer.typ + "/*", "*/*"}
	for _, want := range candidates {
		for _, key := range componentNames(content) {
			for entry := range mediaRanges(key) {
				if entry == want {
					return key, content[key]
				}
			}
		}
	}
	return "", nil
}
