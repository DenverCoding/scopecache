// json.go — single seam through which all of `package scopecache`
// reaches a JSON library. Every other file calls jsonMarshal,
// jsonUnmarshal, or jsonNewDecoder instead of importing
// encoding/json directly. To swap libraries (back to stdlib for
// debugging, to a different fast-json impl, or to revert) change
// the var-block below — no call site needs to know.
//
// Why goccy/go-json:
//
//   - Drop-in API replacement for encoding/json: honors
//     MarshalJSON/UnmarshalJSON interfaces, same struct-tag rules,
//     same NewDecoder shape.
//   - Measured 2026-05-14 (BenchmarkUnmarshal_itemsRequest): on the
//     /warm and /rebuild input path with 10000 items, goccy posts
//     1.85 ms vs stdlib 8.10 ms — 4.37x faster, 6.25 ms saved per
//     request.
//   - No code generation, no go-generate step, no generated files
//     in the tree, no drift risk on struct changes.
//   - See CLAUDE.md "stdlib-only" section for the pre-1.0 exception
//     rationale, including why mailru/easyjson (the obvious
//     alternative) was rejected.
//
// Files that still use encoding/json directly:
//
//   - types.go and handlers_read.go: import the package for the
//     json.RawMessage type alias and the appendJSONString
//     fallback for control characters. Neither calls Marshal /
//     Unmarshal. Leave as-is.
//   - *_test.go files: test code may use either; not part of the
//     production hot path.

package scopecache

import (
	stdjson "encoding/json"

	gojson "github.com/goccy/go-json"
)

// jsonMarshal is the production envelope-encoder. Used by
// writeJSONResponse (every non-cap-protected handler response) and
// emitEvent (the events-stream auto-populate path).
var jsonMarshal = gojson.Marshal

// jsonUnmarshal is the production envelope-decoder. Used by any
// call site that wants the buffered all-at-once form. The streaming
// decoder below is preferred for HTTP bodies because it gives us
// "extra trailing JSON" detection via Decode + EOF.
var jsonUnmarshal = gojson.Unmarshal

// jsonNewDecoder mirrors json.NewDecoder. decodeBody uses it for
// every POST handler so MaxBytesReader-capped input streams without
// a separate buffering step.
var jsonNewDecoder = gojson.NewDecoder

// jsonValid mirrors json.Valid: confirms the bytes are syntactically
// well-formed JSON. validatePayload calls this on every write — every
// /append, /upsert, /update, /counter_add.
//
// NOTE: pointed at stdlib, NOT goccy/go-json. Measurement 2026-05-14
// (BenchmarkValid_*): stdlib 70 ns / 0 allocs vs goccy 769 ns / 17
// allocs on a small object — 11x slower. Goccy's Valid appears to
// allocate transient state we don't need; stdlib's is a hand-tuned
// byte-scanning validator. This is the one call where stdlib wins
// against goccy, and we want the fast path on every write.
var jsonValid = stdjson.Valid
