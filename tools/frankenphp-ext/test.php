<?php
// test.php — minimal demo of the in-process PHP→scopecache call.
//
// Every extension entry-point now returns a JSON STRING — byte-identical
// to the HTTP-endpoint response body. PHP-side, the common pattern is
// `json_decode($result, true)` to get an associative array.
//
// What this proves:
//   - PHP can write to the cache via HTTP /append (goes through the
//     scopecache caddymodule's *Gateway).
//   - PHP can read the same item back via scopecache_get() (which
//     LookupGateway("default")s the SAME *Gateway — no second cache).
//   - A miss returns the same envelope shape as HTTP — hit=false,
//     item=null — not a PHP null.
//   - Writes return the HTTP /append envelope with created=true plus
//     the assigned seq under 'item.seq'.
//
// Must run inside a binary that has the scopecache caddymodule
// configured in the Caddyfile (so Provision() ran and registered
// the gateway). Plain `frankenphp php-cli test.php` will show NULL
// because no caddymodule is active — the extension's LookupGateway
// returns nil. To exercise the actual round-trip:
//
//   ./dist/frankenphp run --config Caddyfile.bench
//   curl http://localhost:8080/test.php

header('Content-Type: text/plain; charset=utf-8');

echo "=== scopecache PHP extension — registry-aware demo ===\n\n";

// Step 1: seed an item via HTTP /append. This goes through the
// scopecache caddymodule's *Gateway — the SAME gateway the extension
// will read from via LookupGateway("default").
$seed_body = json_encode([
    'scope'   => 'demo',
    'id'      => 'hello',
    'payload' => ['greeting' => 'hi from scopecache, via cgo, in-process'],
]);
$ch = curl_init('http://127.0.0.1:8080/append');
curl_setopt_array($ch, [
    CURLOPT_POST           => true,
    CURLOPT_HTTPHEADER     => ['Content-Type: application/json'],
    CURLOPT_POSTFIELDS     => $seed_body,
    CURLOPT_RETURNTRANSFER => true,
]);
curl_exec($ch);
$seed_status = curl_getinfo($ch, CURLINFO_HTTP_CODE);
unset($ch);
$seed_note = ($seed_status == 409) ? " (already existed, OK)" : "";
echo "Seed POST /append          -> HTTP $seed_status$seed_note\n\n";

// Step 2: hit — pre-seeded item. Returns the /get envelope as a JSON
// string. json_decode is the canonical PHP-side conversion.
$got_raw = scopecache_get('demo', 'hello');
echo "scopecache_get('demo', 'hello') -> ";
var_dump(json_decode($got_raw, true));

// Step 3: miss — unknown id within a known scope. hit=false envelope.
$miss_raw = scopecache_get('demo', 'no-such-item');
echo "scopecache_get('demo', 'no-such-item') -> ";
var_dump(json_decode($miss_raw, true));

// Step 4: miss — unknown scope entirely. Same shape; cache treats
// unknown scope identically to unknown id.
$miss_scope_raw = scopecache_get('no-such-scope', 'hello');
echo "scopecache_get('no-such-scope', 'hello') -> ";
var_dump(json_decode($miss_scope_raw, true));

echo "\n=== scopecache_append envelope ===\n\n";

// Append from PHP side: returns /append envelope with created=true
// and item.seq cache-assigned (>= 1). We use a fresh id each run so
// we don't collide with prior seeds.
$append_id = 'php-write-' . bin2hex(random_bytes(4));
$append_payload = json_encode(['written' => 'from PHP via scopecache_append']);
$append_env_raw = scopecache_append('demo', $append_id, $append_payload);
echo "scopecache_append('demo', '$append_id', ...) -> ";
var_dump(json_decode($append_env_raw, true));

// Read back what we just wrote.
$readback_raw = scopecache_get('demo', $append_id);
echo "scopecache_get('demo', '$append_id') -> ";
var_dump(json_decode($readback_raw, true));

// Append into a never-seen scope creates it implicitly (scopecache
// has no separate scope-create primitive).
$bootstrap_id = 'bootstrap-' . bin2hex(random_bytes(4));
$bootstrap_env_raw = scopecache_append('php-side-scope', $bootstrap_id, '"hi"');
echo "scopecache_append('php-side-scope', '$bootstrap_id', '\"hi\"') -> ";
var_dump(json_decode($bootstrap_env_raw, true));

echo "\n=== scopecache_tail envelope ===\n\n";

// Hit: tail returns /tail envelope with items[]. The seeded 'demo'
// scope should have at least the seed plus the two appends above.
$tail_hit_raw = scopecache_tail('demo', 5);
echo "scopecache_tail('demo', 5) -> ";
var_dump(json_decode($tail_hit_raw, true));

// Miss: tail on unknown scope returns the same shape with hit=false,
// items=[].
$tail_miss_raw = scopecache_tail('no-such-scope', 5);
echo "scopecache_tail('no-such-scope', 5) -> ";
var_dump(json_decode($tail_miss_raw, true));

echo "\n=== scopecache_get_payload (no envelope) ===\n\n";

// Payload-only read — the lowest-overhead path: skip envelope build
// entirely, return raw payload bytes. Equivalent to GET /render.
$payload_raw = scopecache_get_payload('demo', 'hello');
echo "scopecache_get_payload('demo', 'hello') -> ";
var_dump($payload_raw);

echo "\n";
echo "Every line above shows the JSON envelope decoded into a PHP\n";
echo "associative array via json_decode. The raw return from each\n";
echo "scopecache_* function is a STRING — byte-identical to the\n";
echo "matching HTTP endpoint response. Use json_decode when you need\n";
echo "the array shape; pass the string straight through when you are\n";
echo "forwarding to another HTTP layer or storing it as-is.\n";
echo "\n";
echo "scopecache_get_payload skips the envelope entirely and returns\n";
echo "just the raw payload bytes — the cheapest single-item read.\n";
echo "\n";
echo "If all lines show NULL: no caddymodule was loaded. Check that\n";
echo "your Caddyfile has a `scopecache { ... }` block and that the\n";
echo "binary is running via `frankenphp run --config <file>` (not\n";
echo "`frankenphp php-cli`, which skips Provision entirely).\n";
