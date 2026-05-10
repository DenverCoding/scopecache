<?php
// test.php — minimal demo of the in-process PHP→scopecache call.
//
// What this proves:
//   - PHP can call scopecache_get() as a regular function (no curl,
//     no HTTP, no JSON encoding on either side).
//   - The byte-string returned is the verbatim Payload that
//     scopecache stored — what HTTP clients see under .item.payload.
//   - Miss returns null (?string in the export-php directive).
//
// The extension pre-seeds one item in init() so this script has
// something to read out of the box. Once scopecache_append lands as
// a second extension function, the seeding moves into PHP and this
// file shows a real round-trip.

header('Content-Type: text/plain; charset=utf-8');

echo "=== scopecache PHP extension — single-function prototype ===\n\n";

// 1. Hit: pre-seeded item from the extension's init() goroutine.
$payload = scopecache_get('demo', 'hello');
echo "scopecache_get('demo', 'hello') -> ";
var_dump($payload);

// 2. Miss: unknown id returns PHP null.
$miss = scopecache_get('demo', 'no-such-item');
echo "scopecache_get('demo', 'no-such-item') -> ";
var_dump($miss);

// 3. Miss: unknown scope returns PHP null.
$miss_scope = scopecache_get('no-such-scope', 'hello');
echo "scopecache_get('no-such-scope', 'hello') -> ";
var_dump($miss_scope);

echo "\n";
echo "If the first call returned a JSON-shaped string and the other two\n";
echo "returned NULL, the extension is wired correctly.\n";
