// miss_header_test.go — verifies the Scopecache-Miss response header
// (MissHeader) is set exactly on a data miss: a /get, /render, /tail,
// /head, /update, or /delete that found no item. It must be absent on
// hits, on validation errors, on always-storing writes (/append), and
// on observability endpoints (/scopelist).

package scopecache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// doMissRequest dispatches one request and returns the status code and
// the MissHeader value ("" when the header is absent).
func doMissRequest(t *testing.T, h http.Handler, method, path, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Header().Get(MissHeader)
}

func TestMissHeader(t *testing.T) {
	h, _ := newTestHandler(1000)

	// Seed scope "things" with id "a" so the hit cases have a target.
	if code, _ := doMissRequest(t, h, http.MethodPost, "/append",
		`{"scope":"things","id":"a","payload":{"v":1}}`); code != http.StatusOK {
		t.Fatalf("seed /append: code = %d, want 200", code)
	}

	// Cases run in registration order: every read/update hit case
	// targets id "a" before "delete hit" removes it.
	cases := []struct {
		name     string
		method   string
		path     string
		body     string
		wantCode int
		wantMiss bool
	}{
		{"get hit", http.MethodGet, "/get?scope=things&id=a", "", 200, false},
		{"get miss", http.MethodGet, "/get?scope=things&id=nope", "", 200, true},
		{"render hit", http.MethodGet, "/render?scope=things&id=a", "", 200, false},
		{"render miss", http.MethodGet, "/render?scope=things&id=nope", "", 404, true},
		{"tail hit", http.MethodGet, "/tail?scope=things", "", 200, false},
		{"tail miss", http.MethodGet, "/tail?scope=void", "", 200, true},
		{"head hit", http.MethodGet, "/head?scope=things", "", 200, false},
		{"head miss", http.MethodGet, "/head?scope=void", "", 200, true},
		{"update hit", http.MethodPost, "/update", `{"scope":"things","id":"a","payload":{"v":2}}`, 200, false},
		{"update miss", http.MethodPost, "/update", `{"scope":"things","id":"nope","payload":{"v":2}}`, 200, true},
		{"delete miss", http.MethodPost, "/delete", `{"scope":"things","id":"nope"}`, 200, true},
		{"delete hit", http.MethodPost, "/delete", `{"scope":"things","id":"a"}`, 200, false},
		{"append never misses", http.MethodPost, "/append", `{"scope":"things","id":"b","payload":{"v":1}}`, 200, false},
		{"validation error is not a miss", http.MethodGet, "/get?scope=things", "", 400, false},
		{"scopelist is not a miss", http.MethodGet, "/scopelist", "", 200, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, miss := doMissRequest(t, h, tc.method, tc.path, tc.body)
			if code != tc.wantCode {
				t.Errorf("status = %d, want %d", code, tc.wantCode)
			}
			switch {
			case tc.wantMiss && miss != "true":
				t.Errorf("%s = %q, want %q", MissHeader, miss, "true")
			case !tc.wantMiss && miss != "":
				t.Errorf("%s = %q, want absent", MissHeader, miss)
			}
		})
	}
}
