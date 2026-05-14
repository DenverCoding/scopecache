package scopecache

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestComplexJSONRoundtrip — POST /warm with a deeply-nested
// payload containing every JSON corner goccy could conceivably
// trip on (escaped quotes, backslashes, unicode, scientific
// notation, nulls, empty arrays, mixed-type values), then GET /tail
// and verify the bytes come back byte-for-byte identical.
//
// Confidence-builder for the goccy swap: if anything mangles, this
// test fails loudly.
func TestComplexJSONRoundtrip(t *testing.T) {
	h, _ := newTestHandler(10000)

	complex := `{
        "escaped_quote": "say \"hi\"",
        "backslash":     "a\\b",
        "unicode":       "café — résumé — 北京 — 🚀",
        "scientific":    1.0e-3,
        "negative_exp":  -1.5e-4,
        "huge":          1.7976931348623157e308,
        "true":          true,
        "false":         false,
        "null":          null,
        "empty_array":   [],
        "empty_object":  {},
        "nested": {
            "level1": {"level2": {"level3": {"level4": {"level5": "deep"}}}}
        },
        "array_of_mixed": [1, "two", 3.14, true, null, {"k":"v"}, [1,2,3]],
        "string_with_solidus": "a/b/c",
        "control_chars":  "tab\there\nnewline"
    }`

	// Round 1: POST /warm
	warmBody := map[string]any{
		"items": []map[string]any{
			{"scope": "rt-test", "id": "item-1", "payload": json.RawMessage(complex)},
		},
	}
	body, _ := json.Marshal(warmBody)
	req := httptest.NewRequest(http.MethodPost, "/warm", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST /warm: status %d body=%s", rec.Code, rec.Body.String())
	}

	// Round 2: GET /tail
	req2 := httptest.NewRequest(http.MethodGet, "/tail?scope=rt-test&limit=10", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("GET /tail: status %d body=%s", rec2.Code, rec2.Body.String())
	}
	tailBody := rec2.Body.Bytes()

	// Verify each tricky token survived intact.
	tail := string(tailBody)
	checks := map[string]string{
		`"escaped_quote":"say \"hi\""`:                                "escaped quote",
		`"backslash":"a\\b"`:                                          "backslash",
		`"café — résumé — 北京 — 🚀"`:                                    "unicode",
		`"scientific":1.0e-3`:                                         "scientific notation (small)",
		`"negative_exp":-1.5e-4`:                                      "negative scientific",
		`"huge":1.7976931348623157e308`:                               "huge float (max safe)",
		`"empty_array":[]`:                                            "empty array",
		`"empty_object":{}`:                                           "empty object",
		`"level5":"deep"`:                                             "deep nesting (5 levels)",
		`"control_chars":"tab\there\nnewline"`:                        "control chars",
		`"array_of_mixed":[1,"two",3.14,true,null,{"k":"v"},[1,2,3]]`: "mixed-type array",
	}
	for needle, label := range checks {
		if !strings.Contains(tail, needle) {
			t.Errorf("round-trip lost %s: needle %q missing from tail body", label, needle)
		}
	}

	// Also: the payload we wrote back must be valid JSON.
	var rt struct {
		Items []struct {
			Payload map[string]any `json:"payload"`
		} `json:"items"`
	}
	if err := json.Unmarshal(tailBody, &rt); err != nil {
		t.Fatalf("could not re-parse tail body: %v\nbody=%s", err, tailBody)
	}
	if len(rt.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(rt.Items))
	}
	if rt.Items[0].Payload["nested"].(map[string]any)["level1"].(map[string]any)["level2"].(map[string]any)["level3"].(map[string]any)["level4"].(map[string]any)["level5"] != "deep" {
		t.Errorf("deep nesting traversal failed")
	}
}
