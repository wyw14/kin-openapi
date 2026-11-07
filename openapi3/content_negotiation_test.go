package openapi3

import (
	"errors"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAcceptQuality_String(t *testing.T) {
	t.Parallel()
	for q, want := range map[AcceptQuality]string{
		0:    "0",
		1:    "0.001",
		10:   "0.01",
		100:  "0.1",
		500:  "0.5",
		1000: "1",
	} {
		require.Equal(t, want, q.String(), "q=%d", q)
	}
}

func TestParseAccept(t *testing.T) {
	t.Parallel()

	t.Run("missing or empty means accept all", func(t *testing.T) {
		entries, err := ParseAccept()
		require.NoError(t, err)
		require.Nil(t, entries)

		entries, err = ParseAccept("", "   ")
		require.NoError(t, err)
		require.Nil(t, entries)
	})

	t.Run("entries, params and positions", func(t *testing.T) {
		// A comma and a semicolon hide inside quoted values and must not
		// split the entry or its parameters.
		entries, err := ParseAccept(`application/json; profile="v,1"; charset="u;tf-8", text/plain ; q=0.1`)
		require.NoError(t, err)
		require.Len(t, entries, 2)

		require.Equal(t, "application/json", entries[0].MediaRange())
		require.Equal(t, "v,1", entries[0].Params["profile"])
		require.Equal(t, "u;tf-8", entries[0].Params["charset"])
		require.Equal(t, DefaultAcceptQuality, entries[0].Quality)
		require.Equal(t, 0, entries[0].Header)
		require.Equal(t, 0, entries[0].Index)

		require.Equal(t, "text/plain", entries[1].MediaRange())
		require.Equal(t, AcceptQuality(100), entries[1].Quality)
		require.Equal(t, 1, entries[1].Index)
	})

	t.Run("multiple headers merge in order", func(t *testing.T) {
		entries, err := ParseAccept("application/json;q=0.1", "application/xml;q=0.9")
		require.NoError(t, err)
		require.Len(t, entries, 2)
		require.Equal(t, 0, entries[0].Header)
		require.Equal(t, 1, entries[1].Header)
		require.Equal(t, "application/json", entries[0].MediaRange())
		require.Equal(t, "application/xml", entries[1].MediaRange())
		require.Equal(t, AcceptQuality(900), entries[1].Quality)
	})

	t.Run("wildcards and case folding", func(t *testing.T) {
		entries, err := ParseAccept("APPLICATION/*, */*")
		require.NoError(t, err)
		require.Equal(t, "application", entries[0].Type)
		require.Equal(t, "*", entries[0].Subtype)
		require.Equal(t, "*", entries[1].Type)
		require.Equal(t, "*", entries[1].Subtype)
	})

	t.Run("valid q grammar stays exact", func(t *testing.T) {
		cases := map[string]AcceptQuality{
			"application/json;q=0":     0,
			"application/json;q=0.":    0,
			"application/json;q=0.000": 0,
			"application/json;q=0.001": 1,
			"application/json;q=0.01":  10,
			"application/json;q=0.1":   100,
			"application/json;q=0.999": 999,
			"application/json;q=1":     1000,
			"application/json;q=1.":    1000,
			"application/json;q=1.000": 1000,
			`application/json;q="0.5"`: 500,
			"application/json":         DefaultAcceptQuality,
		}
		for header, want := range cases {
			entries, err := ParseAccept(header)
			require.NoError(t, err, header)
			require.Equal(t, want, entries[0].Quality, header)
		}
	})

	t.Run("illegal q is a parse error", func(t *testing.T) {
		for _, header := range []string{
			"application/json;q=2",
			"application/json;q=1.1",
			"application/json;q=1.01",
			"application/json;q=1.001",
			"application/json;q=1.0001",
			"application/json;q=0.1234",
			"application/json;q=.5",
			"application/json;q=abc",
			"application/json;q=-1",
			"application/json;q=0.5.0",
			"application/json;q=01",
		} {
			_, err := ParseAccept(header)
			var pe *AcceptParseError
			require.ErrorAs(t, err, &pe, header)
			require.Equal(t, 0, pe.Header, header)
		}
	})

	t.Run("other malformed headers", func(t *testing.T) {
		for _, header := range []string{
			"application",                  // missing slash
			"application/",                 // empty subtype
			"*/json",                       // wildcard type with concrete subtype
			"appli cation/json",            // space in token
			"application/json;q",           // parameter without value
			"application/json;=v",          // empty parameter name
			"application/json,",            // trailing empty entry
			"application/json;q=0.5;q=0.6", // duplicate q
			`application/json;x="a`,        // unterminated quote
			`application/json;x="a"b`,      // trailing junk after quote
		} {
			_, err := ParseAccept(header)
			var pe *AcceptParseError
			require.ErrorAs(t, err, &pe, header)
		}
	})
}

func TestContent_NegotiateResponse(t *testing.T) {
	t.Parallel()

	jsonMT := NewMediaType()
	xmlMT := NewMediaType()
	appWildMT := NewMediaType()
	allMT := NewMediaType()
	plusMT := NewMediaType()
	profileV1 := NewMediaType()
	profileV2 := NewMediaType()

	content := Content{
		"application/json":            jsonMT,
		"application/xml":             xmlMT,
		"application/*":               appWildMT,
		"*/*":                         allMT,
		"application/vnd.x+json":      plusMT,
		"application/json;profile=v1": profileV1,
		"application/json;profile=v2": profileV2,
	}

	type want struct {
		contentType string
		mt          *MediaType
		key         string
		quality     AcceptQuality
		entry       string // MediaRange of the deciding entry, "" when nil
		noEntry     bool
		err         any // *AcceptParseError, *OfferNotDocumentedError, etc.
		notAccept   bool
	}
	tests := []struct {
		name    string
		content Content
		offers  []string
		accept  []string
		want    want
	}{
		{
			name:    "exact q=0 overrides permissive wildcard",
			content: Content{"application/json": jsonMT, "*/*": allMT},
			offers:  []string{"application/json"},
			accept:  []string{"application/json;q=0, */*;q=1"},
			want:    want{notAccept: true},
		},
		{
			name:    "exact q=0 with wildcard still allows another offer",
			content: Content{"application/json": jsonMT, "application/*": appWildMT, "*/*": allMT},
			offers:  []string{"application/json", "application/xml"},
			accept:  []string{"application/json;q=0, */*;q=1"},
			want: want{
				contentType: "application/xml",
				mt:          appWildMT,
				key:         "application/*",
				quality:     1000,
				entry:       "*/*",
			},
		},
		{
			name:   "two profile offers, higher q wins",
			offers: []string{"application/json;profile=v1", "application/json;profile=v2"},
			accept: []string{"application/json;profile=v2;q=0.9, application/json;profile=v1;q=0.8"},
			want: want{
				contentType: "application/json;profile=v2",
				mt:          profileV2,
				key:         "application/json;profile=v2",
				quality:     900,
				entry:       "application/json",
			},
		},
		{
			name:   "profile parameter values keep their case",
			offers: []string{"application/json;profile=v2"},
			accept: []string{"application/json;profile=V2"},
			want:   want{notAccept: true},
		},
		{
			name:    "profile covered by parameterless key",
			content: Content{"application/json": jsonMT},
			offers:  []string{`application/json;profile="v,1"`},
			accept:  []string{`application/json;profile="v,1"`},
			want: want{
				contentType: `application/json;profile="v,1"`,
				mt:          jsonMT,
				key:         "application/json",
				quality:     1000,
				entry:       "application/json",
			},
		},
		{
			name:   "duplicate ranges take highest q",
			offers: []string{"application/json"},
			accept: []string{"application/json;q=0.5, application/json;q=0.8"},
			want: want{
				contentType: "application/json",
				mt:          jsonMT,
				key:         "application/json",
				quality:     800,
				entry:       "application/json",
			},
		},
		{
			name:   "duplicate ranges rescue a q=0",
			offers: []string{"application/json"},
			accept: []string{"application/json;q=0, application/json"},
			want: want{
				contentType: "application/json",
				mt:          jsonMT,
				key:         "application/json",
				quality:     1000,
				entry:       "application/json",
			},
		},
		{
			name:   "duplicate equal ranges keep the first entry",
			offers: []string{"application/json"},
			accept: []string{"application/json;q=0.8, application/json;q=0.8"},
			want: want{
				contentType: "application/json",
				mt:          jsonMT,
				key:         "application/json",
				quality:     800,
				entry:       "application/json",
			},
		},
		{
			name:   "multiple Accept headers are merged",
			offers: []string{"application/json", "application/xml"},
			accept: []string{"application/json;q=0.1", "application/xml;q=0.9"},
			want: want{
				contentType: "application/xml",
				mt:          xmlMT,
				key:         "application/xml",
				quality:     900,
				entry:       "application/xml",
			},
		},
		{
			name:    "quoted commas and semicolons do not split entries",
			content: Content{"application/json": jsonMT, "*/*": allMT},
			offers:  []string{`application/json;profile="v,1";charset="u;tf-8"`, "text/plain"},
			accept:  []string{`application/json;profile="v,1";charset="u;tf-8"`, "text/plain;q=0.1"},
			want: want{
				contentType: `application/json;profile="v,1";charset="u;tf-8"`,
				mt:          jsonMT,
				key:         "application/json",
				quality:     1000,
				entry:       "application/json",
			},
		},
		{
			name:   "illegal q is a parse error, not */*",
			offers: []string{"application/json"},
			accept: []string{"application/json;q=2"},
			want:   want{err: &AcceptParseError{}},
		},
		{
			name:   "missing Accept accepts all and keeps server order",
			offers: []string{"application/json", "application/xml"},
			want: want{
				contentType: "application/json",
				mt:          jsonMT,
				key:         "application/json",
				quality:     1000,
				noEntry:     true,
			},
		},
		{
			name:   "whitespace-only Accept is like missing",
			offers: []string{"application/xml", "application/json"},
			accept: []string{"   "},
			want: want{
				contentType: "application/xml",
				mt:          xmlMT,
				key:         "application/xml",
				quality:     1000,
				noEntry:     true,
			},
		},
		{
			name:   "all offers at q=0 are not acceptable",
			offers: []string{"application/json"},
			accept: []string{"application/json;q=0"},
			want:   want{notAccept: true},
		},
		{
			name:   "no offered type appears in Accept",
			offers: []string{"application/json"},
			accept: []string{"text/html"},
			want:   want{notAccept: true},
		},
		{
			name:    "undocumented offer errors before negotiation",
			content: Content{"application/json": jsonMT},
			offers:  []string{"text/html"},
			accept:  []string{"this is not valid"},
			want:    want{err: &OfferNotDocumentedError{}},
		},
		{
			name:   "wildcard offer is not concrete",
			offers: []string{"application/*"},
			want:   want{err: &InvalidOfferMediaTypeError{}},
		},
		{
			name:   "no offers is not negotiable",
			offers: nil,
			want:   want{notAccept: true},
		},
		{
			name:    "exact q beats higher wildcard q at equal server order",
			content: Content{"application/json": jsonMT, "*/*": allMT},
			offers:  []string{"application/json"},
			accept:  []string{"*/*;q=1, application/json;q=0.9"},
			want: want{
				contentType: "application/json",
				mt:          jsonMT,
				key:         "application/json",
				quality:     900,
				entry:       "application/json",
			},
		},
		{
			name:    "specificity beats server offer order on q ties",
			content: Content{"application/json": jsonMT, "*/*": allMT},
			offers:  []string{"text/plain", "application/json"},
			accept:  []string{"application/json, */*"},
			want: want{
				contentType: "application/json",
				mt:          jsonMT,
				key:         "application/json",
				quality:     1000,
				entry:       "application/json",
			},
		},
		{
			name:   "accept entry order breaks q ties",
			offers: []string{"application/json", "application/xml"},
			accept: []string{"application/xml, application/json"},
			want: want{
				contentType: "application/xml",
				mt:          xmlMT,
				key:         "application/xml",
				quality:     1000,
				entry:       "application/xml",
			},
		},
		{
			name:   "server offer order breaks the last tie",
			offers: []string{"application/xml", "application/json"},
			accept: []string{"*/*"},
			want: want{
				contentType: "application/xml",
				mt:          xmlMT,
				key:         "application/xml",
				quality:     1000,
				entry:       "*/*",
			},
		},
		{
			name:   "exact thousandths order without floats",
			offers: []string{"application/xml", "application/json"},
			accept: []string{"application/xml;q=0.001, application/json;q=0.002"},
			want: want{
				contentType: "application/json",
				mt:          jsonMT,
				key:         "application/json",
				quality:     2,
				entry:       "application/json",
			},
		},
		{
			name:   "media type names are case-insensitive, output keeps offer spelling",
			offers: []string{"Application/JSON"},
			accept: []string{"application/JSON"},
			want: want{
				contentType: "Application/JSON",
				mt:          jsonMT,
				key:         "application/json",
				quality:     1000,
				entry:       "application/json",
			},
		},
		{
			name:    "+json is not equivalent to application/json",
			content: Content{"application/vnd.x+json": plusMT, "application/json": jsonMT},
			offers:  []string{"application/vnd.x+json"},
			accept:  []string{"application/json"},
			want:    want{notAccept: true},
		},
		{
			name:    "*+json suffix wildcard is not supported",
			content: Content{"application/vnd.x+json": plusMT},
			offers:  []string{"application/vnd.x+json"},
			accept:  []string{"application/*+json"},
			want:    want{notAccept: true},
		},
		{
			name:    "structured suffix still matches exactly",
			content: Content{"application/vnd.x+json": plusMT},
			offers:  []string{"application/vnd.x+json"},
			accept:  []string{"application/vnd.x+json"},
			want: want{
				contentType: "application/vnd.x+json",
				mt:          plusMT,
				key:         "application/vnd.x+json",
				quality:     1000,
				entry:       "application/vnd.x+json",
			},
		},
		{
			name:    "range whose parameter does not match is skipped",
			content: Content{"application/*": appWildMT, "*/*": allMT},
			offers:  []string{"application/json;level=2"},
			accept:  []string{"application/*;level=1;q=1, */*;q=0.5"},
			want: want{
				// level=1 does not match level=2, so the more specific type
				// range cannot decide; the parameterless wildcard does.
				contentType: "application/json;level=2",
				mt:          appWildMT,
				key:         "application/*",
				quality:     500,
				entry:       "*/*",
			},
		},
		{
			name:    "more matching parameters decide effective quality",
			content: Content{"application/*": appWildMT},
			offers:  []string{"application/json;level=2"},
			accept:  []string{"application/*;level=1;q=1, application/*;level=2;q=0.9"},
			want: want{
				contentType: "application/json;level=2",
				mt:          appWildMT,
				key:         "application/*",
				quality:     900,
				entry:       "application/*",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.content
			if c == nil {
				c = content
			}
			got, err := c.NegotiateResponse(tt.offers, tt.accept...)
			switch {
			case tt.want.err != nil:
				require.Error(t, err)
				require.IsType(t, tt.want.err, err)
				require.Nil(t, got)
			case tt.want.notAccept:

				var nae *NotAcceptableError
				require.ErrorAs(t, err, &nae)
				require.True(t, errors.Is(err, ErrNotAcceptable))
				require.Nil(t, got)
			default:
				require.NoError(t, err)
				require.NotNil(t, got)
				require.Equal(t, tt.want.contentType, got.ContentType)
				require.Same(t, tt.want.mt, got.MediaType)
				require.Equal(t, tt.want.key, got.ContentKey)
				require.Equal(t, tt.want.quality, got.Quality)
				if tt.want.noEntry {
					require.Nil(t, got.AcceptEntry)
				} else {
					require.NotNil(t, got.AcceptEntry)
					require.Equal(t, tt.want.entry, got.AcceptEntry.MediaRange())
				}
			}
		})
	}

	t.Run("inputs are not modified", func(t *testing.T) {
		offers := []string{"application/json", "application/xml"}
		accept := []string{"application/xml;q=0.9"}
		before := len(content)
		_, err := content.NegotiateResponse(offers, accept...)
		require.NoError(t, err)
		require.Len(t, content, before)
		require.Same(t, jsonMT, content["application/json"])
		require.Equal(t, []string{"application/json", "application/xml"}, offers)
	})
}

// TestContent_NegotiateResponse_DecidingEntry pins which Accept entry wins
// for an offer when several ranges have the same specificity.
func TestContent_NegotiateResponse_DecidingEntry(t *testing.T) {
	t.Parallel()

	c := Content{"application/json": NewMediaType()}
	offers := []string{"application/json"}

	// Highest q among equally specific ranges decides.
	got, err := c.NegotiateResponse(offers, "application/json;q=0.5, application/json;q=0.8")
	require.NoError(t, err)
	require.Equal(t, AcceptQuality(800), got.Quality)
	require.Equal(t, 1, got.AcceptEntry.Index)

	// Equal q keeps the earlier entry.
	got, err = c.NegotiateResponse(offers, "application/json;q=0.8, application/json;q=0.8")
	require.NoError(t, err)
	require.Equal(t, 0, got.AcceptEntry.Index)
}

// TestContent_NegotiateResponse_MapOrderIndependence rebuilds the same
// Content map with keys inserted in random (and comma-listed) orders and
// checks the choice is always the same for fixed offers and Accept headers.
func TestContent_NegotiateResponse_MapOrderIndependence(t *testing.T) {
	t.Parallel()

	mtJSON := NewMediaType()
	mtApp := NewMediaType()
	mtAll := NewMediaType()

	build := func(keys []string) Content {
		c := make(Content, 3)
		for _, k := range keys {
			switch k {
			case "text/csv, application/json":
				c[k] = mtJSON
			case "application/*":
				c[k] = mtApp
			case "*/*":
				c[k] = mtAll
			}
		}
		return c
	}
	keys := []string{"text/csv, application/json", "application/*", "*/*"}
	offers := []string{"application/json", "application/xml", "text/plain", "text/csv"}

	cases := []struct {
		accept string
		want   string
		mt     *MediaType
	}{
		{"*/*", "application/json", mtJSON},
		{"text/plain", "text/plain", mtAll},
		{"application/xml, application/json", "application/xml", mtApp},
		{"application/json, application/xml", "application/json", mtJSON},
		{"text/csv;q=0.9, */*;q=0.1", "text/csv", mtJSON},
	}

	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 200; iter++ {
		shuffled := append([]string(nil), keys...)
		rng.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		c := build(shuffled)
		for _, tc := range cases {
			got, err := c.NegotiateResponse(offers, tc.accept)
			require.NoError(t, err, "iter %d, accept %q", iter, tc.accept)
			require.Equal(t, tc.want, got.ContentType, "iter %d, accept %q", iter, tc.accept)
			require.Same(t, tc.mt, got.MediaType, "iter %d, accept %q", iter, tc.accept)
		}
	}
}
