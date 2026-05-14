//go:build unix

package scopecache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// benchHTTPServer wires up a real HTTP server on a Unix domain socket,
// backed by a store populated via the same benchStore used by the
// in-process benchmarks. The returned client has a keep-alive pool
// sized for parallel benchmarks — which is what a real application
// does when it reuses http.Client across goroutines.
func benchHTTPServer(b *testing.B, numScopes, itemsPerScope, payloadBytes int) (
	client *http.Client,
	scopes []string,
	ids []string,
	cleanup func(),
) {
	b.Helper()

	store, scopes, ids := benchStore(b, numScopes, itemsPerScope, payloadBytes)

	api := NewAPI(&Gateway{store: store}, APIConfig{})
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	dir, err := os.MkdirTemp("", "scopecache-bench-")
	if err != nil {
		b.Fatalf("mkdtemp: %v", err)
	}
	sockPath := filepath.Join(dir, "sc.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		os.RemoveAll(dir)
		b.Fatalf("listen unix: %v", err)
	}

	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(ln) }()

	client = &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
			MaxIdleConns:        1024,
			MaxIdleConnsPerHost: 1024,
			IdleConnTimeout:     0,
		},
	}

	// One warmup request so connection setup isn't charged to the first b.N
	// iteration of the caller's benchmark loop.
	if len(scopes) > 0 && len(ids) > 0 {
		resp, err := client.Get("http://sock/get?scope=" + scopes[0] + "&id=" + ids[0])
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}

	cleanup = func() {
		// Force-close rather than graceful Shutdown: the latter blocks on
		// idle keep-alive connections in the client pool, which stalls the
		// next benchmark. Close both sides explicitly and the socket file.
		client.CloseIdleConnections()
		_ = server.Close()
		_ = ln.Close()
		_ = os.RemoveAll(dir)
	}
	return client, scopes, ids, cleanup
}

// doGET executes a GET, drains the body, and fails the benchmark on a non-2xx
// response. Draining is mandatory so the keep-alive pool can reuse the conn.
func doGET(b *testing.B, client *http.Client, url string) {
	resp, err := client.Get(url)
	if err != nil {
		b.Fatalf("GET %s: %v", url, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
}

// BenchmarkHTTP_GetByID measures an end-to-end /get request over a Unix
// socket — the full path a real client pays: syscall + net/http + JSON
// envelope marshal. Compare against BenchmarkStore_GetByID (in-process,
// ~30 ns) to see what HTTP framing actually costs.
func BenchmarkHTTP_GetByID(b *testing.B) {
	client, scopes, ids, cleanup := benchHTTPServer(b, 100, 1000, 512)
	defer cleanup()
	numScopes := len(scopes)
	itemsPerScope := len(ids) / numScopes

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		scope := scopes[i%numScopes]
		id := ids[i%itemsPerScope]
		doGET(b, client, "http://sock/get?scope="+scope+"&id="+id)
	}
}

// BenchmarkHTTP_GetByID_Parallel runs /get concurrently across GOMAXPROCS
// goroutines, each reusing the shared http.Client (and therefore its
// keep-alive pool) — exactly how an application server serves concurrent
// inbound requests.
func BenchmarkHTTP_GetByID_Parallel(b *testing.B) {
	client, scopes, ids, cleanup := benchHTTPServer(b, 100, 1000, 512)
	defer cleanup()
	numScopes := len(scopes)
	itemsPerScope := len(ids) / numScopes

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			scope := scopes[i%numScopes]
			id := ids[i%itemsPerScope]
			doGET(b, client, "http://sock/get?scope="+scope+"&id="+id)
			i++
		}
	})
}

// BenchmarkHTTP_RenderByID measures /render — raw payload bytes, no JSON
// envelope. Diff against BenchmarkHTTP_GetByID to see the pure envelope cost.
func BenchmarkHTTP_RenderByID(b *testing.B) {
	client, scopes, ids, cleanup := benchHTTPServer(b, 100, 1000, 512)
	defer cleanup()
	numScopes := len(scopes)
	itemsPerScope := len(ids) / numScopes

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		scope := scopes[i%numScopes]
		id := ids[i%itemsPerScope]
		doGET(b, client, "http://sock/render?scope="+scope+"&id="+id)
	}
}

// BenchmarkHTTP_RenderByID_StringPayload_Parallel hits /render on items
// whose payload is a JSON string ("<html>...") — the path that triggers
// the per-hit json.Unmarshal + []byte cast in handleRender. The non-
// string variant (benchHTTPServer's default payload is a JSON object)
// skips that branch entirely. Diff against
// BenchmarkHTTP_RenderByID_Parallel to measure how much the unmarshal
// adds; if it's >5-10% there's a case for pre-decoding at write-time.
func BenchmarkHTTP_RenderByID_StringPayload_Parallel(b *testing.B) {
	// Seed a single scope where every payload is a JSON-encoded string,
	// roughly 512 bytes after encoding (matches benchStore's payload size).
	store := newStore(Config{
		ScopeMaxItems: 100_000,
		MaxStoreBytes: 1 << 30,
		MaxItemBytes:  1 << 20,
	})
	const scope = "html"
	buf, err := store.getOrCreateScope(scope)
	if err != nil {
		b.Fatalf("getOrCreateScope: %v", err)
	}
	rawHTML := bytes.Repeat([]byte("x"), 500)
	payload, _ := json.Marshal("<html>" + string(rawHTML) + "</html>")
	const n = 1000
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := "page-" + itoa(i)
		ids[i] = id
		if _, err := buf.appendItem(Item{Scope: scope, ID: id, Payload: payload}); err != nil {
			b.Fatalf("appendItem: %v", err)
		}
	}

	api := NewAPI(&Gateway{store: store}, APIConfig{})
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	dir, err := os.MkdirTemp("", "scopecache-bench-")
	if err != nil {
		b.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(dir)
	sockPath := filepath.Join(dir, "sc.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(ln) }()
	defer server.Close()
	defer ln.Close()
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
			MaxIdleConns:        1024,
			MaxIdleConnsPerHost: 1024,
		},
	}
	defer client.CloseIdleConnections()
	// Warmup
	doGET(b, client, "http://sock/render?scope="+scope+"&id="+ids[0])

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			doGET(b, client, "http://sock/render?scope="+scope+"&id="+ids[i%n])
			i++
		}
	})
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

// BenchmarkHTTP_RenderByID_Parallel is the parallel counterpart — the
// fastest realistic read path scopecache exposes.
func BenchmarkHTTP_RenderByID_Parallel(b *testing.B) {
	client, scopes, ids, cleanup := benchHTTPServer(b, 100, 1000, 512)
	defer cleanup()
	numScopes := len(scopes)
	itemsPerScope := len(ids) / numScopes

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			scope := scopes[i%numScopes]
			id := ids[i%itemsPerScope]
			doGET(b, client, "http://sock/render?scope="+scope+"&id="+id)
			i++
		}
	})
}

// BenchmarkHTTP_Tail_5k mirrors scripts/bench.sh's tail-5k mode:
// 100-item scope, 5 KiB payloads, /tail?limit=100. Each response is
// ~500 KiB of marshalled JSON. The dominant cost here is NOT the
// JSON build (already hand-rolled via appendItemJSON) but the
// 503-KiB heap allocation per request from writeItemsResponse's
// pre-grow buf. This bench surfaces that cost so the sync.Pool
// fix (or any alternative) can be measured against a baseline.
func BenchmarkHTTP_Tail_5k(b *testing.B) {
	client, scopes, _, cleanup := benchHTTPServer(b, 1, 100, 5*1024)
	defer cleanup()
	scope := scopes[0]

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		doGET(b, client, "http://sock/tail?scope="+scope+"&limit=100")
	}
}

// BenchmarkHTTP_Head10 models the "load the last 10 messages in this thread"
// pattern — a small batch read rather than a single item. Uses limit=10.
func BenchmarkHTTP_Head10(b *testing.B) {
	client, scopes, _, cleanup := benchHTTPServer(b, 100, 1000, 512)
	defer cleanup()
	numScopes := len(scopes)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		scope := scopes[i%numScopes]
		doGET(b, client, "http://sock/head?scope="+scope+"&limit=10")
	}
}

// BenchmarkHTTP_Append measures the write path end-to-end. It rotates
// through the pre-populated bench:* scopes so the benchmark does not pay
// the per-scope allocation cost (each new scope pre-allocates its items
// slice to defaultMaxItems capacity, which is unrelated to request cost).
// Included because the write-buffer use case the README describes was
// otherwise unmeasured.
func BenchmarkHTTP_Append(b *testing.B) {
	client, scopes, _, cleanup := benchHTTPServer(b, 100, 1000, 512)
	defer cleanup()
	numScopes := len(scopes)

	payloadFiller := make([]byte, 512)
	for i := range payloadFiller {
		payloadFiller[i] = 'y'
	}
	payloadRaw, _ := json.Marshal(map[string]string{"data": string(payloadFiller)})

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		scope := scopes[i%numScopes]
		body, _ := json.Marshal(map[string]any{
			"scope":   scope,
			"payload": json.RawMessage(payloadRaw),
		})
		resp, err := client.Post("http://sock/append", "application/json", bytes.NewReader(body))
		if err != nil {
			b.Fatalf("POST /append: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			b.Fatalf("POST /append: status %d", resp.StatusCode)
		}
	}
}

// BenchmarkHTTP_Warm_* posts a {"items":[...]} body of N entries
// to /warm, end-to-end via the live caddyscope-shaped HTTP path.
// The body is built once and posted b.N times; each iteration goes
// through MaxBytesReader, the JSON decoder (json.go -> goccy on
// main, encoding/json on earlier versions), and the store's
// replaceScopes path. SetBytes reports throughput in MB/s.
//
// Items spread across 50 scopes so the lock contention pattern is
// realistic — a single-scope warm at 10k items would serialize on
// one buf.mu and not exercise the multi-scope reservation logic.

func BenchmarkHTTP_Warm_1000(b *testing.B)  { benchHTTPWarm(b, 1000) }
func BenchmarkHTTP_Warm_10000(b *testing.B) { benchHTTPWarm(b, 10000) }

// buildComplexPayload returns a realistic-ish deeply-nested JSON
// payload that exercises goccy's parser on the awkward corners:
// 6-7 levels of nesting, mixed types, arrays of objects, escaped
// strings (quotes, backslashes, unicode), null + bool + numeric
// in the same object, scientific notation. ~2 KiB per payload.
func buildComplexPayload(idx int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
        "id": %d,
        "user": {
            "name": "user-%d",
            "profile": {
                "bio": "Multi-line\nbio with \"quotes\" and \\backslash and unicode é ü.",
                "links": [
                    {"type": "twitter", "handle": "@user%d", "verified": true},
                    {"type": "github", "handle": "u-%d", "verified": false},
                    {"type": "blog", "handle": null}
                ],
                "preferences": {
                    "theme": "dark",
                    "notifications": {
                        "email": true,
                        "push": false,
                        "rules": [
                            {"event": "reply",  "threshold": 0.95},
                            {"event": "mention","threshold": 1.0e-3},
                            {"event": "like",   "threshold": null}
                        ]
                    }
                }
            },
            "stats": {
                "posts": %d,
                "followers": %d,
                "score": 3.14159265358979,
                "ratio": -1.5e-4,
                "history": [{"day":"2026-01-01","count":42},{"day":"2026-01-02","count":7}]
            }
        },
        "tags": ["tag-a","tag-b","tag-with-spaces","tag/with/slash","tag\"quoted\""],
        "metadata": {
            "created_at": "2026-05-14T08:00:00.123456Z",
            "permissions": {
                "read": ["public","friends"],
                "write": ["self"],
                "admin": []
            },
            "version": "1.0.0",
            "deleted": false,
            "archived_at": null
        }
    }`, idx, idx, idx, idx, idx*7, idx*13))
}

// BenchmarkHTTP_Warm_Complex_* mirrors BenchmarkHTTP_Warm_* but each
// item's Payload is a 2 KiB nested-JSON document with mixed types,
// escaped strings, scientific notation, and nulls. Goccy must parse
// it cleanly on the way IN; the cache must round-trip the bytes
// without mangling on the way OUT.
//
// Per-payload size ~2 KiB → body ~2 MB at 1000 items, ~20 MB at
// 10000 items. Goes through maxBulkBytes guard at decodeBody.
func BenchmarkHTTP_Warm_Complex_100(b *testing.B)  { benchHTTPWarmComplex(b, 100) }
func BenchmarkHTTP_Warm_Complex_1000(b *testing.B) { benchHTTPWarmComplex(b, 1000) }

func benchHTTPWarmComplex(b *testing.B, n int) {
	client, _, _, cleanup := benchHTTPServer(b, 1, 1, 8)
	defer cleanup()

	items := make([]map[string]any, n)
	for i := 0; i < n; i++ {
		items[i] = map[string]any{
			"scope":   fmt.Sprintf("warm-c-%d", i%50),
			"id":      fmt.Sprintf("item-%d", i),
			"payload": buildComplexPayload(i),
		}
	}
	body, _ := json.Marshal(map[string]any{"items": items})

	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		resp, err := client.Post("http://sock/warm", "application/json", bytes.NewReader(body))
		if err != nil {
			b.Fatalf("POST /warm: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			b.Fatalf("POST /warm: status %d", resp.StatusCode)
		}
	}
}

func benchHTTPWarm(b *testing.B, n int) {
	client, _, _, cleanup := benchHTTPServer(b, 1, 1, 8) // tiny seed; we replace
	defer cleanup()

	items := make([]map[string]any, n)
	for i := 0; i < n; i++ {
		items[i] = map[string]any{
			"scope":   fmt.Sprintf("warm-%d", i%50),
			"id":      fmt.Sprintf("item-%d", i),
			"payload": json.RawMessage(fmt.Sprintf(`{"idx":%d,"data":"row-%d"}`, i, i)),
		}
	}
	body, _ := json.Marshal(map[string]any{"items": items})

	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		resp, err := client.Post("http://sock/warm", "application/json", bytes.NewReader(body))
		if err != nil {
			b.Fatalf("POST /warm: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			b.Fatalf("POST /warm: status %d", resp.StatusCode)
		}
	}
}
