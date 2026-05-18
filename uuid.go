// uuid.go owns the UUID type — a UUIDv7 held as a raw 16-byte value —
// plus minting and strict UUIDv7 validation. Stdlib only
// (math/rand/v2 + time + bit manipulation), no third-party dependency,
// consistent with the root module's stdlib-only rule.
//
// Every Item carries a `uuid`: a UUIDv7 the cache mints on single-item
// writes (/append, /upsert-create, /counter_add-create) and on the
// /warm // /rebuild input items that arrive without one. It is the
// stable link back to the source-of-truth row.
//
// In-memory form. UUID is [16]byte, NOT a string. The canonical
// 36-char hex string exists only at the two boundaries: produced on
// the wire by appendUUIDJSON, parsed back by UUID.UnmarshalJSON /
// parseUUIDv7. Holding the raw 16 bytes inline in Item — and using
// UUID directly as the byUUID map key — removes the per-minted-item
// string heap object (~48 bytes) that otherwise lived for the item's
// whole lifetime and had to be GC-scan-marked on every cycle. The
// struct field is the same width either way (a string header is also
// 16 bytes), so the only thing dropped is the separate allocation.
//
// Layout (RFC 9562 §5.7): bytes 0-5 are a 48-bit unix-millisecond
// timestamp; byte 6 high nibble is version 7; byte 8 top two bits are
// the variant (0b10). The remaining 74 bits — the 12-bit rand_a field
// and the 62-bit rand_b field — are filled entirely with randomness.
// That is RFC 9562 §5.7 "method 1".
//
// No counter, no lock, no shared state. newUUIDv7 is a pure function:
// one clock read plus two math/rand/v2 draws. An earlier draft used a
// store-wide monotonic counter so that same-millisecond UUIDs were
// strictly ordered; that counter needed a synchronised increment,
// which funnelled every concurrent write through one cache line and
// cost ~20% append throughput. Nothing in the cache needs
// within-millisecond UUID ordering — firstUUID/lastUUID track insert
// order directly, byUUID is an exact-match index, /delete_up_to
// resolves a boundary UUID to a seq — so the counter was pure cost.
//
// Uniqueness is probabilistic, not structural — and that is safe
// here. Two UUIDs minted in different milliseconds can never collide
// (distinct timestamp prefix). Within one millisecond, 74 random bits
// give a birthday-collision probability of ~N²/2^75; at the cache's
// measured peak (~200k writes/s ≈ 200 per ms) that is ~1e-18 per
// millisecond — unobservable over any realistic process lifetime. A
// cache is disposable and rebuildable besides; even an (effectively
// impossible) collision would only make one item unreachable by uuid,
// never corrupt state.
//
// Why math/rand/v2, not crypto/rand. A `uuid` is an item identity,
// not a secret or a capability — possessing a scope name grants
// access, never knowing an item's uuid. A per-mint crypto/rand read
// costs a getrandom syscall (~500 ns measured); math/rand/v2's
// auto-seeded ChaCha8 source delivers strong-quality bits in a few ns
// with no syscall. Its global source is goroutine-safe, so newUUIDv7
// is safe to call concurrently with no synchronisation of its own.

package scopecache

import (
	"errors"
	mathrand "math/rand/v2"
	"time"
)

// UUID is a UUIDv7 stored as its raw 16 bytes. The zero value means
// "no uuid" — an item minted in an orphan store-less buffer, or a
// not-yet-minted create write. JSON marshalling/unmarshalling go
// through the canonical 36-char lowercase-hex string; in memory the
// cache only ever moves the 16 bytes around.
type UUID [16]byte

// errInvalidUUIDv7 is returned by parseUUIDv7 / UUID.UnmarshalJSON for
// any input that is not a canonical lowercase-hex UUIDv7 string.
var errInvalidUUIDv7 = errors.New("the 'uuid' field must be a canonical lowercase UUIDv7 string")

// uuidStringLen is the length of a canonical UUID string (36 chars).
// The validators' per-item size pre-checks charge it as the uuid's
// byte-accounting cost (a conservative over-estimate of the 16-byte
// in-memory form — see approxItemSize).
const uuidStringLen = 36

// hexDigits indexes a nibble to its lowercase-hex character.
const hexDigits = "0123456789abcdef"

// newUUIDv7 mints a fresh UUIDv7. Pure function, no shared state —
// safe to call concurrently from any goroutine. See the file header
// for the no-counter / collision argument.
func newUUIDv7() UUID {
	ms := uint64(time.Now().UnixMilli())
	randA := mathrand.Uint64() // only the low 12 bits are used (rand_a)
	randB := mathrand.Uint64() // only the low 62 bits are used (rand_b)

	var b UUID
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = 0x70 | byte((randA>>8)&0x0F)  // version 7 + rand_a high nibble
	b[7] = byte(randA)                   // rand_a low byte
	b[8] = 0x80 | byte((randB>>56)&0x3F) // variant 0b10 + rand_b
	b[9] = byte(randB >> 48)
	b[10] = byte(randB >> 40)
	b[11] = byte(randB >> 32)
	b[12] = byte(randB >> 24)
	b[13] = byte(randB >> 16)
	b[14] = byte(randB >> 8)
	b[15] = byte(randB)
	return b
}

// IsZero reports whether u is the zero value ("no uuid").
func (u UUID) IsZero() bool { return u == UUID{} }

// isValidV7 reports whether u's version and variant nibbles are a
// well-formed UUIDv7 (version 7, variant 0b10). The zero value is NOT
// valid v7. Pure in-memory check, no allocation — used to guard
// Go-API callers that hand-build an Item (the HTTP path's shape is
// already enforced by UUID.UnmarshalJSON).
func (u UUID) isValidV7() bool {
	return u[6]>>4 == 0x7 && u[8]>>6 == 0b10
}

// String renders u as the canonical 36-char lowercase-hex string.
// The zero value renders as the all-zeros uuid; callers that want an
// empty string for "no uuid" use wireUUID.
func (u UUID) String() string { return formatUUID(u) }

// MarshalJSON renders u as a JSON string — the canonical 36-char form,
// or "" for the zero value. The hot wire paths (Item.MarshalJSON,
// AppendItemJSON, writeGetResponse) bypass this and call appendUUIDJSON
// directly; this method covers any generic json.Marshal of a UUID.
func (u UUID) MarshalJSON() ([]byte, error) {
	return appendUUIDJSON(make([]byte, 0, uuidStringLen+2), u), nil
}

// UnmarshalJSON parses a JSON string into u with strict UUIDv7
// validation. `null` and "" both leave the zero value (the "absent"
// sentinel — /warm and /rebuild mint one, /append rejects a present
// uuid). Any non-canonical string is errInvalidUUIDv7.
func (u *UUID) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	if len(data) < 2 || data[0] != '"' || data[len(data)-1] != '"' {
		return errInvalidUUIDv7
	}
	s := data[1 : len(data)-1]
	if len(s) == 0 {
		return nil
	}
	parsed, err := parseUUIDv7(string(s))
	if err != nil {
		return err
	}
	*u = parsed
	return nil
}

// wireUUID renders u for the wire as a plain string: the canonical
// 36-char form, or "" for the zero value. Used where a UUID crosses
// into a string-typed field (writeAck, writeEvent) outside the
// hand-rolled byte emitters.
func wireUUID(u UUID) string {
	if u.IsZero() {
		return ""
	}
	return formatUUID(u)
}

// appendUUIDHex appends u as 36 lowercase-hex chars with hyphens at
// the canonical 8-4-4-4-12 positions. No allocation — writes straight
// into dst.
func appendUUIDHex(dst []byte, u UUID) []byte {
	for i := 0; i < 16; i++ {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			dst = append(dst, '-')
		}
		dst = append(dst, hexDigits[u[i]>>4], hexDigits[u[i]&0x0F])
	}
	return dst
}

// appendUUIDJSON appends u as a JSON string token: `"<36 hex>"`, or
// `""` for the zero value. Allocation-free — the canonical emitter for
// the `uuid` / `event_uuid` / `first_uuid` / `last_uuid` wire keys.
func appendUUIDJSON(dst []byte, u UUID) []byte {
	if u.IsZero() {
		return append(dst, '"', '"')
	}
	dst = append(dst, '"')
	dst = appendUUIDHex(dst, u)
	return append(dst, '"')
}

// formatUUID renders u as a 36-char lowercase-hex UUID string.
func formatUUID(u UUID) string {
	return string(appendUUIDHex(make([]byte, 0, uuidStringLen), u))
}

// parseUUIDv7 strictly validates a canonical UUIDv7 string and returns
// its 16 bytes. Rejects uppercase hex, wrong length, misplaced hyphens,
// non-hex digits, and any version nibble other than 7 or variant other
// than 0b10.
func parseUUIDv7(s string) (UUID, error) {
	var b UUID
	if len(s) != 36 {
		return b, errInvalidUUIDv7
	}
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return b, errInvalidUUIDv7
	}
	src := 0
	for i := 0; i < 16; i++ {
		if src == 8 || src == 13 || src == 18 || src == 23 {
			src++ // skip the hyphen
		}
		hi, ok1 := hexVal(s[src])
		lo, ok2 := hexVal(s[src+1])
		if !ok1 || !ok2 {
			return b, errInvalidUUIDv7
		}
		b[i] = hi<<4 | lo
		src += 2
	}
	if b[6]>>4 != 0x7 {
		return b, errInvalidUUIDv7 // version must be 7
	}
	if b[8]>>6 != 0b10 {
		return b, errInvalidUUIDv7 // variant must be 0b10
	}
	return b, nil
}

// isValidUUIDv7 reports whether s is a canonical UUIDv7 string.
func isValidUUIDv7(s string) bool {
	_, err := parseUUIDv7(s)
	return err == nil
}

// hexVal decodes one lowercase-hex digit; ok is false for uppercase or
// any non-hex byte (strict canonical form).
func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	default:
		return 0, false
	}
}
