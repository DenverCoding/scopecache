<?php
// bench-sprint1.php — measures the three cgo entry points side-by-side:
// scopecache_get, scopecache_tail, scopecache_append. Single-thread,
// pre-warmed, fixed payload.
//
// Each bench uses its own scope so seed state for one does not perturb
// the others. Tail's scope is pre-seeded to TAIL_LIMIT items. Append
// uses seq-only items into a freshly-wiped scope; iter is capped so
// the buffer stays under the default per-scope budget.
//
//   GET /bench-sprint1.php?iter=100000&warmup=5000&tail_limit=10
//
// Tail-array construction cost scales with limit AND payload size;
// vary either to trace the curve.

header('Content-Type: text/plain; charset=utf-8');
set_time_limit(0);

$ITER       = (int)($_GET['iter']       ?? 100000);
$WARMUP     = (int)($_GET['warmup']     ?? 5000);
$TAIL_LIMIT = (int)($_GET['tail_limit'] ?? 10);

$payload = json_encode(['greeting' => 'hi from scopecache, via cgo, in-process']);
$payload_bytes = strlen($payload);

// --- Wipe + reseed the bench scopes --------------------------------------
function wipe_scope(string $scope): void {
    $ch = curl_init('http://127.0.0.1:8080/delete_scope');
    curl_setopt_array($ch, [
        CURLOPT_POST           => true,
        CURLOPT_HTTPHEADER     => ['Content-Type: application/json'],
        CURLOPT_POSTFIELDS     => json_encode(['scope' => $scope]),
        CURLOPT_RETURNTRANSFER => true,
    ]);
    curl_exec($ch);
    unset($ch);
}

$get_scope    = 'bench-sp1-get';
$tail_scope   = 'bench-sp1-tail';
$append_scope = 'bench-sp1-append';

wipe_scope($get_scope);
wipe_scope($tail_scope);
wipe_scope($append_scope);

// Seed scopecache_get's scope with one item.
scopecache_append($get_scope, 'item', $payload);

// Seed scopecache_tail's scope with TAIL_LIMIT items.
for ($i = 0; $i < $TAIL_LIMIT; $i++) {
    scopecache_append($tail_scope, "item-$i", $payload);
}

// Sanity probes.
if (scopecache_get($get_scope, 'item') === null) die("seed FAILED: get scope empty\n");
$probe_tail = scopecache_tail($tail_scope, $TAIL_LIMIT);
if (!is_array($probe_tail) || count($probe_tail) !== $TAIL_LIMIT) {
    die("seed FAILED: tail scope has " . (is_array($probe_tail) ? count($probe_tail) : 'null') . " items, expected $TAIL_LIMIT\n");
}

// --- Report header --------------------------------------------------------
printf("payload bytes : %d\n",  $payload_bytes);
printf("tail_limit    : %d\n",  $TAIL_LIMIT);
printf("iterations    : %d (warmup %d)\n", $ITER, $WARMUP);
echo "\n";
printf("%-38s | %-12s | %-15s\n", "op", "per call", "throughput");
printf("%s\n", str_repeat('-', 75));

// --- scopecache_get -------------------------------------------------------
for ($i = 0; $i < $WARMUP; $i++) scopecache_get($get_scope, 'item');
$t0 = hrtime(true);
for ($i = 0; $i < $ITER; $i++) { $r = scopecache_get($get_scope, 'item'); }
$ns_get = hrtime(true) - $t0;

// --- scopecache_tail ------------------------------------------------------
for ($i = 0; $i < $WARMUP; $i++) scopecache_tail($tail_scope, $TAIL_LIMIT);
$t0 = hrtime(true);
for ($i = 0; $i < $ITER; $i++) { $r = scopecache_tail($tail_scope, $TAIL_LIMIT); }
$ns_tail = hrtime(true) - $t0;

// --- scopecache_append (seq-only) ----------------------------------------
// Each call creates a new item; iter is capped so the scope's items
// slice does not blow past the default per-scope item cap (~512K).
// 30k items × ~100 B = ~3 MB — well within the default 256 MB store cap.
$APPEND_ITER   = min($ITER, 30000);
$APPEND_WARMUP = min($WARMUP, 500);
for ($i = 0; $i < $APPEND_WARMUP; $i++) scopecache_append($append_scope, '', $payload);
$t0 = hrtime(true);
for ($i = 0; $i < $APPEND_ITER; $i++) { $s = scopecache_append($append_scope, '', $payload); }
$ns_append = hrtime(true) - $t0;

// --- Print rows -----------------------------------------------------------
$row = function(string $label, float $ns, int $iter) {
    $per = $ns / $iter;
    $qps = 1e9 * $iter / $ns;
    printf("%-38s | %-12s | %-15s\n",
        $label,
        sprintf("%.1f ns", $per),
        number_format($qps, 0, '.', ' ') . ' /s'
    );
};

$row("scopecache_get",                              $ns_get,    $ITER);
$row("scopecache_tail (limit=$TAIL_LIMIT)",         $ns_tail,   $ITER);
$row("scopecache_append (seq-only, n=$APPEND_ITER)", $ns_append, $APPEND_ITER);

echo "\n";
printf("scopecache_tail per-element overhead : %.1f ns/elt\n",
    ($ns_tail / $ITER - $ns_get / $ITER) / $TAIL_LIMIT);
printf("scopecache_append vs scopecache_get  : %.1fx slower\n",
    ($ns_append / $APPEND_ITER) / ($ns_get / $ITER));
