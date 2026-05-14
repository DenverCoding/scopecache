package scopecache

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWireBuilders_ByteIdentity — for each public byte-builder
// (AppendGetResponseJSON / AppendHeadResponseJSON / AppendTailResponseJSON
// / AppendScopeListResponseJSON) verify the bytes match the HTTP
// handler's response body byte-for-byte across hit / miss / multi-item
// scenarios.
//
// Why: the FrankenPHP extension (and any other in-process Go caller)
// produces JSON envelopes via the public byte-builders; HTTP clients
// receive the handler's writeGetResponse / writeItemsResponse /
// writeScopeListResponse output. Two paths to one wire format.
//
// The test runs both paths against the SAME gateway so seq/ts values
// are identical — comparing bytes from two parallel gateways would
// differ on those fields even with the same input.
func TestWireBuilders_ByteIdentity(t *testing.T) {
	gw := NewGateway(Config{
		ScopeMaxItems: 10000,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
	})
	api := NewAPI(gw, APIConfig{})
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	// Seed: normal item, second item to give /head and /tail a multi-row
	// shape, plus a seq-only item in a second scope (so /scopelist returns
	// two entries and AppendItemJSON's `"id":null` branch gets exercised).
	if _, err := gw.Warm(map[string][]Item{
		"sc-a": {
			{Scope: "sc-a", ID: "item-1", Payload: []byte(`{"v":1}`)},
			{Scope: "sc-a", ID: "item-2", Payload: []byte(`"hi"`)},
		},
		"sc-b": {
			{Scope: "sc-b", Payload: []byte(`42`)}, // seq-only, no id
		},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cases := []struct {
		name string
		path string
		// build returns the bytes produced by the public builder for the
		// SAME query the path encodes; both must observe the same gateway
		// state (= same seq/ts) for byte-equality to hold.
		build func() []byte
	}{
		{
			name: "get-hit",
			path: "/get?scope=sc-a&id=item-1",
			build: func() []byte {
				item, found := gw.GetByID("sc-a", "item-1")
				if !found {
					return AppendGetResponseJSON(nil, nil)
				}
				return AppendGetResponseJSON(nil, &item)
			},
		},
		{
			name:  "get-miss",
			path:  "/get?scope=sc-a&id=does-not-exist",
			build: func() []byte { return AppendGetResponseJSON(nil, nil) },
		},
		{
			name: "get-by-seq-no-id",
			path: "/get?scope=sc-b&seq=1",
			build: func() []byte {
				item, found := gw.GetBySeq("sc-b", 1)
				if !found {
					return AppendGetResponseJSON(nil, nil)
				}
				return AppendGetResponseJSON(nil, &item)
			},
		},
		{
			name: "head-multi",
			path: "/head?scope=sc-a&limit=10",
			build: func() []byte {
				items, truncated, _ := gw.Head("sc-a", 0, 10)
				return AppendHeadResponseJSON(nil, items, truncated)
			},
		},
		{
			name:  "head-empty-scope",
			path:  "/head?scope=does-not-exist&limit=10",
			build: func() []byte { return AppendHeadResponseJSON(nil, nil, false) },
		},
		{
			name: "tail-multi",
			path: "/tail?scope=sc-a&limit=10",
			build: func() []byte {
				items, truncated, _ := gw.Tail("sc-a", 10, 0)
				return AppendTailResponseJSON(nil, items, truncated, 0)
			},
		},
		{
			name: "scopelist-all",
			path: "/scopelist?limit=100",
			build: func() []byte {
				entries, truncated := gw.ScopeList("", "", 100)
				return AppendScopeListResponseJSON(nil, entries, truncated)
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, c.path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("HTTP %s: status %d body=%s", c.path, rec.Code, rec.Body.String())
			}
			httpBytes := rec.Body.Bytes()
			builderBytes := c.build()
			if !bytes.Equal(httpBytes, builderBytes) {
				t.Errorf("byte-identity broke for %s\nHTTP    : %s\nbuilder : %s", c.name, httpBytes, builderBytes)
			}
		})
	}
}
