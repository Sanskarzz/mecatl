package session_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/internal/session"
)

// TestValidateJSONSubset table-tests each supported keyword (valid + invalid) and the
// fail-open policy for unsupported keywords.
func TestValidateJSONSubset(t *testing.T) {
	cases := []struct {
		name    string
		schema  string
		payload string
		wantErr bool
		// errSub, when set, must appear in the error message.
		errSub string
	}{
		{
			name:    "object with required present",
			schema:  `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
			payload: `{"a":"hi"}`,
		},
		{
			name:    "object missing required",
			schema:  `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
			payload: `{}`,
			wantErr: true,
			errSub:  "required",
		},
		{
			name:    "wrong property type",
			schema:  `{"type":"object","properties":{"a":{"type":"string"}}}`,
			payload: `{"a":123}`,
			wantErr: true,
			errSub:  "property a",
		},
		{
			name:    "type mismatch at root",
			schema:  `{"type":"object"}`,
			payload: `["not","an","object"]`,
			wantErr: true,
			errSub:  "expected type object",
		},
		{
			name:    "integer accepts whole number",
			schema:  `{"type":"integer"}`,
			payload: `42`,
		},
		{
			name:    "integer rejects fractional",
			schema:  `{"type":"integer"}`,
			payload: `4.2`,
			wantErr: true,
		},
		{
			name:    "number accepts fractional",
			schema:  `{"type":"number"}`,
			payload: `4.2`,
		},
		{
			name:    "boolean ok",
			schema:  `{"type":"boolean"}`,
			payload: `true`,
		},
		{
			name:    "null ok",
			schema:  `{"type":"null"}`,
			payload: `null`,
		},
		{
			name:    "type union satisfied by one member",
			schema:  `{"type":["string","null"]}`,
			payload: `null`,
		},
		{
			name:    "type union unsatisfied",
			schema:  `{"type":["string","null"]}`,
			payload: `123`,
			wantErr: true,
		},
		{
			name:    "enum match",
			schema:  `{"enum":["a","b","c"]}`,
			payload: `"b"`,
		},
		{
			name:    "enum miss",
			schema:  `{"enum":["a","b","c"]}`,
			payload: `"z"`,
			wantErr: true,
			errSub:  "enum",
		},
		{
			name:    "array items valid",
			schema:  `{"type":"array","items":{"type":"string"}}`,
			payload: `["x","y"]`,
		},
		{
			name:    "array item type mismatch",
			schema:  `{"type":"array","items":{"type":"string"}}`,
			payload: `["x",7]`,
			wantErr: true,
		},
		{
			name:    "nested object property valid",
			schema:  `{"type":"object","properties":{"inner":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}}}`,
			payload: `{"inner":{"n":3}}`,
		},
		{
			name:    "nested object property invalid",
			schema:  `{"type":"object","properties":{"inner":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}}}`,
			payload: `{"inner":{}}`,
			wantErr: true,
			errSub:  "inner",
		},
		{
			// Unsupported keyword (minLength) is IGNORED — fail-open, not an error.
			name:    "unsupported keyword fails open",
			schema:  `{"type":"string","minLength":100}`,
			payload: `"short"`,
		},
		{
			// A non-object schema validates everything (fail-open).
			name:    "non-object schema fails open",
			schema:  `true`,
			payload: `{"anything":42}`,
		},
		{
			name:    "malformed payload is an error",
			schema:  `{"type":"object"}`,
			payload: `{not json`,
			wantErr: true,
			errSub:  "not valid JSON",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := session.ValidateJSON(json.RawMessage(tc.schema), json.RawMessage(tc.payload))
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil error, got %v", err)
			}
			if tc.errSub != "" && (err == nil || !strings.Contains(err.Error(), tc.errSub)) {
				t.Fatalf("error %v must contain %q", err, tc.errSub)
			}
		})
	}
}
