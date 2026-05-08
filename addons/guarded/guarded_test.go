// Tests for the guarded addon. Covered:
//
//   - method check (non-GET → 405)
//   - missing / malformed Authorization header → 401
//   - bearer token whose capID is not provisioned in `_tokens` → 401
//   - valid token + missing scope query → 400
//   - valid token + scope with pre-populated items → 200, items returned
//     with capID prefix stripped from item.scope
//   - prefix isolation: a token-holder cannot read another token's
//     scope by guessing the unprefixed name
//   - empty-scope hit (provisioned token, scope has no items) →
//     200 + hit=false + count=0
//
// The tests construct a real *Gateway with default Config and exercise
// the addon over httptest. No mocks: the addon's only collaborator is
// the public *Gateway, so a real one is the cheapest fixture.
package guarded

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VeloxCoding/scopecache"
)

// newTestServer wires a fresh *Gateway with default Config behind an
// httptest server that mounts only the guarded routes. Returns the
// server, the gateway (so tests can pre-populate scopes and `_tokens`),
// and a teardown closure.
func newTestServer(t *testing.T) (*httptest.Server, *scopecache.Gateway, func()) {
	t.Helper()
	cfg := scopecache.Config{}.WithDefaults()
	gw := scopecache.NewGateway(cfg)
	mux := http.NewServeMux()
	RegisterRoutes(mux, gw)
	srv := httptest.NewServer(mux)
	return srv, gw, srv.Close
}

// provisionToken issues a token by writing an entry to `_tokens` with
// id=capID(token). Mirrors what an operator does via /upsert.
func provisionToken(t *testing.T, gw *scopecache.Gateway, token string) string {
	t.Helper()
	cid := capID(token)
	_, _, err := gw.Upsert(scopecache.Item{
		Scope:   tokensScope,
		ID:      cid,
		Payload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("provision token: %v", err)
	}
	return cid
}

// appendItem writes a single item to (scope, payload), returning the
// committed item. Pre-populates a scope so the tail handler has
// something to return.
func appendItem(t *testing.T, gw *scopecache.Gateway, scope, payload string) {
	t.Helper()
	_, err := gw.Append(scopecache.Item{
		Scope:   scope,
		Payload: json.RawMessage(payload),
	})
	if err != nil {
		t.Fatalf("append %s: %v", scope, err)
	}
}

// doGet runs a GET against /guarded-tail with the given query and
// optional bearer token, returning the parsed JSON body and status.
func doGet(t *testing.T, srv *httptest.Server, query, bearer string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/guarded-tail?"+query, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("parse body %q: %v", string(body), err)
	}
	return resp.StatusCode, parsed
}

func TestGuardedTail_MethodNotAllowed(t *testing.T) {
	srv, _, teardown := newTestServer(t)
	defer teardown()

	resp, err := srv.Client().Post(srv.URL+"/guarded-tail", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != http.MethodGet {
		t.Errorf("Allow header: got %q, want GET", got)
	}
}

func TestGuardedTail_MissingAuthHeader(t *testing.T) {
	srv, _, teardown := newTestServer(t)
	defer teardown()

	status, body := doGet(t, srv, "scope=foo", "")
	if status != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", status)
	}
	if body["ok"] != false {
		t.Errorf("ok: got %v, want false", body["ok"])
	}
}

func TestGuardedTail_NonBearerScheme(t *testing.T) {
	srv, _, teardown := newTestServer(t)
	defer teardown()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/guarded-tail?scope=foo", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", resp.StatusCode)
	}
}

func TestGuardedTail_UnknownToken(t *testing.T) {
	srv, _, teardown := newTestServer(t)
	defer teardown()

	// No tokens provisioned → any bearer token is unknown.
	status, body := doGet(t, srv, "scope=foo", "totally-fabricated-token")
	if status != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", status)
	}
	if body["error"] != "invalid or unknown token" {
		t.Errorf("error message: got %v, want 'invalid or unknown token'", body["error"])
	}
}

func TestGuardedTail_ValidToken_MissingScope(t *testing.T) {
	srv, gw, teardown := newTestServer(t)
	defer teardown()

	provisionToken(t, gw, "tok-A")
	status, body := doGet(t, srv, "", "tok-A")
	if status != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", status)
	}
	if body["ok"] != false {
		t.Errorf("ok: got %v, want false", body["ok"])
	}
}

func TestGuardedTail_ValidToken_EmptyScope(t *testing.T) {
	srv, gw, teardown := newTestServer(t)
	defer teardown()

	provisionToken(t, gw, "tok-A")
	// Token valid, scope name shape valid, but no items written under
	// the prefixed scope yet → hit=false, count=0.
	status, body := doGet(t, srv, "scope=foo", "tok-A")
	if status != http.StatusOK {
		t.Errorf("status: got %d, want 200", status)
	}
	if body["hit"] != false {
		t.Errorf("hit: got %v, want false", body["hit"])
	}
	if body["count"].(float64) != 0 {
		t.Errorf("count: got %v, want 0", body["count"])
	}
}

func TestGuardedTail_ValidToken_ReturnsPrefixStrippedItems(t *testing.T) {
	srv, gw, teardown := newTestServer(t)
	defer teardown()

	cid := provisionToken(t, gw, "tok-A")
	appendItem(t, gw, cid+":foo", `{"v":1}`)
	appendItem(t, gw, cid+":foo", `{"v":2}`)

	status, body := doGet(t, srv, "scope=foo", "tok-A")
	if status != http.StatusOK {
		t.Errorf("status: got %d, want 200", status)
	}
	if body["hit"] != true {
		t.Errorf("hit: got %v, want true", body["hit"])
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items: not a slice: %T", body["items"])
	}
	if len(items) != 2 {
		t.Fatalf("items count: got %d, want 2", len(items))
	}
	for i, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("items[%d] not an object: %T", i, raw)
		}
		// scope is `omitempty` — when stripping leaves "foo" we expect
		// "foo" on the wire, not the prefixed form.
		if item["scope"] != "foo" {
			t.Errorf("items[%d].scope: got %v, want 'foo' (capID prefix should be stripped)", i, item["scope"])
		}
	}
}

func TestGuardedTail_PrefixIsolation_TokenCannotReachOtherTokensScope(t *testing.T) {
	srv, gw, teardown := newTestServer(t)
	defer teardown()

	cidA := provisionToken(t, gw, "tok-A")
	provisionToken(t, gw, "tok-B")
	appendItem(t, gw, cidA+":secrets", `{"a":1}`)

	// tok-B asks for scope=secrets. Real lookup is on
	// `<cidB>:secrets` which has no items → hit=false, isolation
	// holds. tok-B never sees A's data.
	status, body := doGet(t, srv, "scope=secrets", "tok-B")
	if status != http.StatusOK {
		t.Errorf("status: got %d, want 200", status)
	}
	if body["hit"] != false {
		t.Errorf("hit: got %v, want false (B must not see A's secrets)", body["hit"])
	}
	if body["count"].(float64) != 0 {
		t.Errorf("count: got %v, want 0", body["count"])
	}
}

func TestGuardedTail_PrefixIsolation_TokenCannotReachUnprefixedScope(t *testing.T) {
	srv, gw, teardown := newTestServer(t)
	defer teardown()

	provisionToken(t, gw, "tok-A")
	// Items written to a bare scope (no capID prefix) — should be
	// unreachable to any guarded-tail caller because the addon always
	// prepends the capID.
	appendItem(t, gw, "secrets", `{"a":1}`)

	status, body := doGet(t, srv, "scope=secrets", "tok-A")
	if status != http.StatusOK {
		t.Errorf("status: got %d, want 200", status)
	}
	if body["hit"] != false {
		t.Errorf("hit: got %v, want false (unprefixed scope must be unreachable)", body["hit"])
	}
}

func TestGuardedTail_LimitAndOffset(t *testing.T) {
	srv, gw, teardown := newTestServer(t)
	defer teardown()

	cid := provisionToken(t, gw, "tok-A")
	for i := 0; i < 5; i++ {
		appendItem(t, gw, cid+":foo", `{"i":1}`)
	}

	// limit=2 returns 2 items, truncated reflects more available.
	status, body := doGet(t, srv, "scope=foo&limit=2", "tok-A")
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200", status)
	}
	items, _ := body["items"].([]any)
	if len(items) != 2 {
		t.Errorf("limit=2: got %d items, want 2", len(items))
	}
	if body["truncated"] != true {
		t.Errorf("truncated: got %v, want true (5 items, limit=2)", body["truncated"])
	}

	// offset=4, limit=2 returns the last 1 item, no truncation.
	status, body = doGet(t, srv, "scope=foo&limit=2&offset=4", "tok-A")
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200", status)
	}
	items, _ = body["items"].([]any)
	if len(items) != 1 {
		t.Errorf("offset=4 limit=2: got %d items, want 1", len(items))
	}
	if body["offset"].(float64) != 4 {
		t.Errorf("offset echo: got %v, want 4", body["offset"])
	}
}

func TestGuardedTail_BadLimit(t *testing.T) {
	srv, gw, teardown := newTestServer(t)
	defer teardown()

	provisionToken(t, gw, "tok-A")
	status, _ := doGet(t, srv, "scope=foo&limit=-1", "tok-A")
	if status != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400 (limit=-1 is invalid)", status)
	}
}

func TestGuardedTail_BadOffset(t *testing.T) {
	srv, gw, teardown := newTestServer(t)
	defer teardown()

	provisionToken(t, gw, "tok-A")
	status, _ := doGet(t, srv, "scope=foo&offset=abc", "tok-A")
	if status != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400 (offset=abc is invalid)", status)
	}
}

func TestCapID_Stable(t *testing.T) {
	a := capID("hello")
	b := capID("hello")
	if a != b {
		t.Errorf("capID not stable: %q vs %q", a, b)
	}
	if len(a) != 43 {
		t.Errorf("capID length: got %d, want 43 (base64url of sha256 without padding)", len(a))
	}
	c := capID("hellp")
	if a == c {
		t.Errorf("capID collision on 1-byte change")
	}
}
