<?php
// validate-sprint1.php — beyond-smoke correctness checks for the
// Sprint 1 functions (scopecache_tail + scopecache_append). Exercises
// multi-element returns, ordering, edge inputs, error sentinels, and
// the cross-call state-sharing contract.
//
// Output is PASS/FAIL per check; validate-sprint1.sh greps it.

header('Content-Type: text/plain; charset=utf-8');
set_time_limit(0);

$pass = 0;
$fail = 0;

function check(string $label, bool $ok, string $detail = ''): void {
    global $pass, $fail;
    if ($ok) {
        echo "PASS  $label\n";
        $pass++;
    } else {
        echo "FAIL  $label" . ($detail !== '' ? "  ($detail)" : '') . "\n";
        $fail++;
    }
}

echo "=== validate-sprint1.php ===\n\n";

// --- 0. Sanity: functions are loaded -----------------------------------------
check("function_exists scopecache_get",    function_exists('scopecache_get'));
check("function_exists scopecache_tail",   function_exists('scopecache_tail'));
check("function_exists scopecache_append", function_exists('scopecache_append'));

// --- 1. Fresh scope: append N items, tail them back --------------------------
// Use a randomised scope so we are not perturbed by other tests' leftovers.
$rand     = bin2hex(random_bytes(4));
$scope    = "validate-$rand";
$N        = 7;
$expected = [];

$seqs = [];
for ($i = 0; $i < $N; $i++) {
    $payload = json_encode(['idx' => $i, 'tag' => "item-$i"]);
    $expected[] = $payload;
    $seq = scopecache_append($scope, "id-$i", $payload);
    $seqs[] = $seq;
}

check("N appends to fresh scope returned positive seqs",
    count(array_filter($seqs, fn($s) => $s >= 1)) === $N,
    "seqs=" . json_encode($seqs));

check("appended seqs are strictly increasing",
    $seqs === array_values(array_unique($seqs)) && $seqs === call_user_func(function($s) {
        $sorted = $s; sort($sorted); return $sorted;
    }, $seqs),
    "seqs=" . json_encode($seqs));

// --- 2. Tail returns all N items, in seq-ascending order ---------------------
$tail = scopecache_tail($scope, 100);
check("tail($scope, 100) returns an array", is_array($tail), "got " . gettype($tail));
check("tail returns exactly N items (=$N)", is_array($tail) && count($tail) === $N,
    "got count=" . (is_array($tail) ? count($tail) : 'n/a'));

if (is_array($tail) && count($tail) === $N) {
    $contents_match = true;
    foreach ($tail as $idx => $payload) {
        if ($payload !== $expected[$idx]) {
            $contents_match = false;
            break;
        }
    }
    check("tail contents match the appended payloads in order", $contents_match);
}

// --- 3. Tail limit smaller than N: returns last `limit` items ----------------
// Gateway.Tail: "newest `limit` items, oldest-first within the window".
// So tail($scope, 3) on 7 items should give items[4..6].
$tail3 = scopecache_tail($scope, 3);
check("tail(scope, 3) returns 3 items", is_array($tail3) && count($tail3) === 3,
    "got count=" . (is_array($tail3) ? count($tail3) : 'n/a'));
if (is_array($tail3) && count($tail3) === 3) {
    $expected_window = array_slice($expected, $N - 3);
    check("tail(scope, 3) returns the newest 3 (oldest-first)", $tail3 === $expected_window);
}

// --- 4. Tail with limit > N: returns all N items, not limit empty slots ------
$tail99 = scopecache_tail($scope, 99);
check("tail(scope, 99) returns N items (not 99 padded)",
    is_array($tail99) && count($tail99) === $N);

// --- 5. Tail with limit=0: empty array (scope exists, asked nothing) ---------
$tail0 = scopecache_tail($scope, 0);
check("tail(scope, 0) returns an array", is_array($tail0));
check("tail(scope, 0) is empty []", is_array($tail0) && count($tail0) === 0);

// --- 6. Tail on a completely unknown scope: NULL -----------------------------
$tail_miss = scopecache_tail("definitely-not-a-scope-$rand", 5);
check("tail(unknown-scope, 5) returns NULL", $tail_miss === null,
    "got " . var_export($tail_miss, true));

// --- 7. Read-back via scopecache_get: items written via append are visible ---
$probe = scopecache_get($scope, "id-3");
check("scopecache_get sees item-3 written via append",
    $probe === $expected[3],
    "got " . var_export($probe, true));

// --- 8. Duplicate id: second append returns 0 (error sentinel) ---------------
$dup_seq = scopecache_append($scope, "id-3", json_encode(['dup' => true]));
check("append of duplicate id returns 0 (error sentinel)", $dup_seq === 0,
    "got seq=$dup_seq");

// --- 9. Original item NOT clobbered by the failed duplicate append -----------
$probe_after_dup = scopecache_get($scope, "id-3");
check("original item-3 survived the duplicate-append rejection",
    $probe_after_dup === $expected[3],
    "got " . var_export($probe_after_dup, true));

// --- 10. Invalid JSON payload: returns 0 -------------------------------------
$invalid_seq = scopecache_append($scope, "invalid-json-id", "this is not JSON");
check("append with non-JSON payload returns 0", $invalid_seq === 0,
    "got seq=$invalid_seq");

// --- 11. Empty payload: returns 0 (validator requires non-empty JSON) --------
$empty_seq = scopecache_append($scope, "empty-id", "");
check("append with empty payload returns 0", $empty_seq === 0,
    "got seq=$empty_seq");

// --- 12. Seq-only append (empty id): assigned seq >= 1, no id collision ------
$seq_only_a = scopecache_append($scope, "", json_encode(['seq_only' => 'a']));
$seq_only_b = scopecache_append($scope, "", json_encode(['seq_only' => 'b']));
check("two seq-only appends both got seq >= 1", $seq_only_a >= 1 && $seq_only_b >= 1,
    "seqs=$seq_only_a,$seq_only_b");
check("the two seq-only appends got DIFFERENT seqs", $seq_only_a !== $seq_only_b,
    "both seqs=$seq_only_a");

// --- 13. After seq-only appends, tail count = N + 2 --------------------------
$tail_after = scopecache_tail($scope, 100);
check("tail after 2 seq-only appends: N+2 items present",
    is_array($tail_after) && count($tail_after) === $N + 2,
    "got count=" . (is_array($tail_after) ? count($tail_after) : 'n/a'));

// --- 14. New scope created lazily by append (no separate create call) --------
$fresh_scope = "lazily-created-$rand";
$lazy_seq = scopecache_append($fresh_scope, "first", json_encode(['first' => 'item']));
check("append into never-before-seen scope created it (seq>=1)", $lazy_seq >= 1,
    "got seq=$lazy_seq");
$lazy_tail = scopecache_tail($fresh_scope, 10);
check("tail of lazily-created scope returns 1 item",
    is_array($lazy_tail) && count($lazy_tail) === 1);

echo "\n=== SUMMARY: $pass pass, $fail fail ===\n";
if ($fail > 0) {
    echo "OVERALL: FAIL\n";
} else {
    echo "OVERALL: PASS\n";
}
