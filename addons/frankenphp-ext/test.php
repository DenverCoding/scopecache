<?php
// test.php — minimal demo of the in-process PHP→scopecache call,
// now registry-aware.
//
// What this proves:
//   - PHP can write to the cache via HTTP /append (goes through the
//     scopecache caddymodule's *Gateway).
//   - PHP can read the same item back via scopecache_get() (which
//     LookupGateway("default")s the SAME *Gateway — no second cache).
//   - A miss on a known scope returns NULL.
//   - A miss on an unknown scope returns NULL.
//
// Must run inside a binary that has the scopecache caddymodule
// configured in the Caddyfile (so Provision() ran and registered
// the gateway). Plain `frankenphp php-cli test.php` will show three
// NULLs because no caddymodule is active — the extension's
// LookupGateway returns nil. To exercise the actual round-trip:
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

// Step 2: hit — pre-seeded item, should return the JSON payload.
$payload = scopecache_get('demo', 'hello');
echo "scopecache_get('demo', 'hello') -> ";
var_dump($payload);

// Step 3: miss — unknown id within a known scope.
$miss = scopecache_get('demo', 'no-such-item');
echo "scopecache_get('demo', 'no-such-item') -> ";
var_dump($miss);

// Step 4: miss — unknown scope entirely.
$miss_scope = scopecache_get('no-such-scope', 'hello');
echo "scopecache_get('no-such-scope', 'hello') -> ";
var_dump($miss_scope);

echo "\n=== scopecache_append (Sprint 1) ===\n\n";

// Append from PHP side. seq is cache-assigned; should be >= 1.
// We use a fresh id each run so we don't collide with prior /append seeds.
$append_id = 'php-write-' . bin2hex(random_bytes(4));
$append_payload = json_encode(['written' => 'from PHP via scopecache_append']);
$seq = scopecache_append('demo', $append_id, $append_payload);
echo "scopecache_append('demo', '$append_id', ...) -> seq=$seq\n";

// Read back what we just wrote — proves the write went through and
// is visible via the same shared *Gateway.
$readback = scopecache_get('demo', $append_id);
echo "scopecache_get('demo', '$append_id') -> ";
var_dump($readback);

// Error path: append to a non-existent scope. scopecache pre-creates
// nothing for user scopes, so appending creates the scope. This should
// succeed too — there is no "scope doesn't exist" error path for
// /append (it just creates). Verify it returns a positive seq.
$bootstrap_id = 'bootstrap-' . bin2hex(random_bytes(4));
$bootstrap_seq = scopecache_append('php-side-scope', $bootstrap_id, '"hi"');
echo "scopecache_append('php-side-scope', '$bootstrap_id', '\"hi\"') -> seq=$bootstrap_seq (created scope)\n";

echo "\n=== scopecache_tail (Sprint 1) ===\n\n";

// Hit: tail an existing scope. Expect array (possibly with the items
// we just appended above).
$tail_hit = scopecache_tail('demo', 5);
echo "scopecache_tail('demo', 5) -> ";
var_dump($tail_hit);

// Miss: tail an unknown scope. Expect NULL (not [] — we distinguish
// "no scope" from "empty scope").
$tail_miss = scopecache_tail('no-such-scope', 5);
echo "scopecache_tail('no-such-scope', 5) -> ";
var_dump($tail_miss);

echo "\n";
echo "If the get block shows one JSON-shaped string and two NULLs, the\n";
echo "extension is correctly sharing state with HTTP clients through the\n";
echo "gateway registry.\n";
echo "\n";
echo "If scopecache_append returned seq>=1 and the immediate scopecache_get\n";
echo "shows the same payload, PHP-side writes are visible via the same\n";
echo "*Gateway as HTTP-side reads.\n";
echo "\n";
echo "If scopecache_tail('demo', 5) returned an array and\n";
echo "scopecache_tail('no-such-scope', 5) returned NULL, the array-return\n";
echo "wire-shape works and the NULL-vs-empty-array distinction holds.\n";
echo "\n";
echo "If ALL calls returned NULL / 0: no caddymodule was loaded. Check that\n";
echo "your Caddyfile has a `scopecache { ... }` block and that the\n";
echo "binary is running via `frankenphp run --config <file>` (not\n";
echo "`frankenphp php-cli`, which skips Provision entirely).\n";
