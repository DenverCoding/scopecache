// serve_http_test.go — ServeHTTP behaviour for the miss-fallthrough
// path: a cache miss is handed to the next Caddy handler when
// MissFallthrough is set, a hit is served from cache, and a write-miss
// reaches the next handler with its request body intact.

package caddymodule

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VeloxCoding/scopecache"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// newMissTestHandler builds a *Handler with a live scopecache mux —
// the same wiring Provision does, minus the caddy.Context ceremony —
// so ServeHTTP can be exercised directly.
func newMissTestHandler(t *testing.T, fallthroughOn bool) *Handler {
	t.Helper()
	gw := scopecache.NewGateway(scopecache.Config{
		ScopeMaxItems: 1000,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
	})
	api := scopecache.NewAPI(gw, scopecache.APIConfig{})
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	return &Handler{mux: mux, MissFallthrough: fallthroughOn}
}

func TestServeHTTP_MissFallthrough(t *testing.T) {
	const nextBody = "handled-by-next"

	// makeNext records the request body the next handler received and
	// writes a marker response, so a test can tell a cache-served
	// response from a fallthrough-served one.
	makeNext := func(seenBody *string) caddyhttp.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) error {
			b, _ := io.ReadAll(r.Body)
			*seenBody = string(b)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, nextBody)
			return nil
		}
	}

	t.Run("read miss falls through to next", func(t *testing.T) {
		h := newMissTestHandler(t, true)
		var seen string
		req := httptest.NewRequest(http.MethodGet, "/get?scope=s&id=missing", nil)
		rec := httptest.NewRecorder()
		if err := h.ServeHTTP(rec, req, makeNext(&seen)); err != nil {
			t.Fatalf("ServeHTTP: %v", err)
		}
		if rec.Body.String() != nextBody {
			t.Errorf("body = %q, want next to have handled it (%q)", rec.Body.String(), nextBody)
		}
		if got := rec.Header().Get(scopecache.MissHeader); got != "" {
			t.Errorf("%s leaked onto the fallthrough response: %q", scopecache.MissHeader, got)
		}
	})

	t.Run("hit is served from cache", func(t *testing.T) {
		h := newMissTestHandler(t, true)
		seed := httptest.NewRequest(http.MethodPost, "/append",
			strings.NewReader(`{"scope":"s","id":"a","payload":{"v":1}}`))
		h.mux.ServeHTTP(httptest.NewRecorder(), seed)

		var seen string
		req := httptest.NewRequest(http.MethodGet, "/get?scope=s&id=a", nil)
		rec := httptest.NewRecorder()
		if err := h.ServeHTTP(rec, req, makeNext(&seen)); err != nil {
			t.Fatalf("ServeHTTP: %v", err)
		}
		if rec.Body.String() == nextBody {
			t.Error("hit was handed to next; want it served from cache")
		}
		if seen != "" {
			t.Errorf("next ran on a hit (saw body %q)", seen)
		}
		if !strings.Contains(rec.Body.String(), `"hit":true`) {
			t.Errorf("cache response = %q, want a hit envelope", rec.Body.String())
		}
	})

	t.Run("write miss reaches next with body intact", func(t *testing.T) {
		h := newMissTestHandler(t, true)
		const updateBody = `{"scope":"s","id":"missing","payload":{"v":9}}`
		var seen string
		req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(updateBody))
		rec := httptest.NewRecorder()
		if err := h.ServeHTTP(rec, req, makeNext(&seen)); err != nil {
			t.Fatalf("ServeHTTP: %v", err)
		}
		if rec.Body.String() != nextBody {
			t.Errorf("body = %q, want next to have handled the write-miss", rec.Body.String())
		}
		if seen != updateBody {
			t.Errorf("next saw body %q, want the original %q", seen, updateBody)
		}
	})

	t.Run("miss is served from cache when fallthrough is off", func(t *testing.T) {
		h := newMissTestHandler(t, false)
		var seen string
		req := httptest.NewRequest(http.MethodGet, "/get?scope=s&id=missing", nil)
		rec := httptest.NewRecorder()
		if err := h.ServeHTTP(rec, req, makeNext(&seen)); err != nil {
			t.Fatalf("ServeHTTP: %v", err)
		}
		if rec.Body.String() == nextBody {
			t.Error("miss fell through with MissFallthrough off")
		}
		if !strings.Contains(rec.Body.String(), `"hit":false`) {
			t.Errorf("response = %q, want the cache miss envelope", rec.Body.String())
		}
		if got := rec.Header().Get(scopecache.MissHeader); got != "true" {
			t.Errorf("%s = %q, want \"true\" on the cache miss response", scopecache.MissHeader, got)
		}
	})

	t.Run("unmatched path falls through regardless", func(t *testing.T) {
		h := newMissTestHandler(t, false)
		var seen string
		req := httptest.NewRequest(http.MethodGet, "/not-scopecache", nil)
		rec := httptest.NewRecorder()
		if err := h.ServeHTTP(rec, req, makeNext(&seen)); err != nil {
			t.Fatalf("ServeHTTP: %v", err)
		}
		if rec.Body.String() != nextBody {
			t.Errorf("body = %q, want next to handle an unmatched path", rec.Body.String())
		}
	})
}
