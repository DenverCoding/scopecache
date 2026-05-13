<?php
// bench-ext-only.php — measures ONLY the cgo path (scopecache_get).
// Used to compare two extension builds against each other. No
// Redis/Memcached deps — runs in any FrankenPHP+scopecache binary.
//
//   GET /bench-ext-only.php?iter=200000&warmup=5000

header('Content-Type: text/plain; charset=utf-8');
set_time_limit(0);

$ITER   = (int)($_GET['iter']   ?? 200000);
$WARMUP = (int)($_GET['warmup'] ?? 5000);

// Seed bench/item if not present.
$seed = json_encode([
    'scope'   => 'bench',
    'id'      => 'item',
    'payload' => ['greeting' => 'hi from scopecache, via cgo, in-process'],
]);
$ch = curl_init('http://127.0.0.1:8080/append');
curl_setopt_array($ch, [
    CURLOPT_POST => true,
    CURLOPT_HTTPHEADER => ['Content-Type: application/json'],
    CURLOPT_POSTFIELDS => $seed,
    CURLOPT_RETURNTRANSFER => true,
]);
curl_exec($ch);
unset($ch);

// Sanity.
$probe = scopecache_get('bench', 'item');
if ($probe === null) { die("setup error: bench/item not seeded\n"); }
$payload_bytes = strlen($probe);

// Warmup.
for ($i = 0; $i < $WARMUP; $i++) { scopecache_get('bench', 'item'); }

// Measure.
$t0 = hrtime(true);
for ($i = 0; $i < $ITER; $i++) { $r = scopecache_get('bench', 'item'); }
$ns = hrtime(true) - $t0;

$per_op_ns = $ns / $ITER;
$qps       = 1e9 * $ITER / $ns;

printf("payload bytes : %d\n",       $payload_bytes);
printf("iterations    : %d (after %d warmup)\n", $ITER, $WARMUP);
printf("total         : %.1f ms\n",  $ns / 1e6);
printf("per call      : %.1f ns\n",  $per_op_ns);
printf("throughput    : %.0f calls/sec\n", $qps);
