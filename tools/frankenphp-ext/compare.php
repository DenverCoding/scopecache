<?php
// compare.php — PHP GET cost across three backends:
//
//   - Redis     (cold = new connection per call; warm = persistent)
//   - Memcached (cold = new client per call;     warm = persistent)
//   - scopecache via the cgo extension (no connection concept)
//
// Each backend is seeded with the same two payloads — small (54 B)
// and big (5 KiB) — and benched at both sizes. compare.sh starts the
// Redis + Memcached side-containers and connects this script to them
// via container names on a private Docker network.

header('Content-Type: text/plain; charset=utf-8');
set_time_limit(0);

$ITER_COLD  = (int)($_GET['iter_cold'] ?? 2000);
$ITER_WARM  = (int)($_GET['iter_warm'] ?? 20000);
$ITER_LOCAL = (int)($_GET['iter_local'] ?? 100000);
$WARMUP     = (int)($_GET['warmup']    ?? 1000);

$REDIS_HOST = $_GET['redis_host']      ?? 'frankenphp-ext-compare-redis';
$REDIS_PORT = (int)($_GET['redis_port'] ?? 6379);
$MEMC_HOST  = $_GET['memc_host']       ?? 'frankenphp-ext-compare-memcached';
$MEMC_PORT  = (int)($_GET['memc_port']  ?? 11211);

// Payloads encoded the way real apps store cache rows — JSON. The
// small one matches bench.php's default; the big one fills to ~5 KiB.
$small = json_encode(['greeting' => 'hi from scopecache, via cgo, in-process']);
$big   = json_encode(str_repeat('a', 5120 - 2));
$keys  = ['small' => $small, 'big' => $big];

// ---------- seed ----------
$r = new Redis();
$r->connect($REDIS_HOST, $REDIS_PORT);
foreach ($keys as $k => $v) { $r->set($k, $v); }
$r->close();

$m = new Memcached();
$m->addServer($MEMC_HOST, $MEMC_PORT);
foreach ($keys as $k => $v) { $m->set($k, $v); }

scopecache_delete_scope('compare');
foreach ($keys as $k => $v) { scopecache_append('compare', $k, $v); }

// Sanity probes.
$probe = scopecache_get('compare', 'small');
if (!is_array($probe) || ($probe['hit'] ?? null) !== true) {
    die("seed FAILED: scopecache probe " . var_export($probe, true) . "\n");
}

// ---------- bench helper ----------
function bench(string $label, int $iter, callable $op): array {
    $t0 = hrtime(true);
    for ($i = 0; $i < $iter; $i++) $op();
    $ns = hrtime(true) - $t0;
    return [
        'label' => $label,
        'iter'  => $iter,
        'per'   => $ns / $iter,
        'qps'   => 1e9 * $iter / $ns,
    ];
}

$rows = [];

foreach (['small' => 'small (54 B)', 'big' => 'big (5 KiB)'] as $k => $label_size) {

    // Redis cold — new connection + get + close, every iter.
    for ($i = 0; $i < min($WARMUP, $ITER_COLD); $i++) {
        $rc = new Redis(); $rc->connect($REDIS_HOST, $REDIS_PORT);
        $rc->get($k); $rc->close();
    }
    $rows[] = bench("Redis cold      $label_size", $ITER_COLD, function() use ($REDIS_HOST, $REDIS_PORT, $k) {
        $rc = new Redis(); $rc->connect($REDIS_HOST, $REDIS_PORT);
        $rc->get($k); $rc->close();
    });

    // Redis warm — one connection re-used.
    $rw = new Redis(); $rw->connect($REDIS_HOST, $REDIS_PORT);
    for ($i = 0; $i < $WARMUP; $i++) $rw->get($k);
    $rows[] = bench("Redis warm      $label_size", $ITER_WARM, function() use ($rw, $k) {
        $rw->get($k);
    });
    $rw->close();

    // Memcached cold — new client + addServer (lazy connect) per iter.
    for ($i = 0; $i < min($WARMUP, $ITER_COLD); $i++) {
        $mc = new Memcached(); $mc->addServer($MEMC_HOST, $MEMC_PORT);
        $mc->get($k);
    }
    $rows[] = bench("Memcached cold  $label_size", $ITER_COLD, function() use ($MEMC_HOST, $MEMC_PORT, $k) {
        $mc = new Memcached(); $mc->addServer($MEMC_HOST, $MEMC_PORT);
        $mc->get($k);
    });

    // Memcached warm — one client re-used.
    $mw = new Memcached(); $mw->addServer($MEMC_HOST, $MEMC_PORT);
    for ($i = 0; $i < $WARMUP; $i++) $mw->get($k);
    $rows[] = bench("Memcached warm  $label_size", $ITER_WARM, function() use ($mw, $k) {
        $mw->get($k);
    });

    // scopecache (cgo) — no connection concept.
    for ($i = 0; $i < $WARMUP; $i++) scopecache_get('compare', $k);
    $rows[] = bench("scopecache cgo  $label_size", $ITER_LOCAL, function() use ($k) {
        scopecache_get('compare', $k);
    });
}

// ---------- report ----------
echo "\n";
printf("%-32s | %-10s | %-14s | %-17s\n", "path", "iter", "per call", "throughput");
printf("%s\n", str_repeat('-', 84));

$prev_size = null;
foreach ($rows as $r) {
    // group separator between small and big.
    if (strpos($r['label'], 'big') !== false && $prev_size === 'small') {
        printf("%s\n", str_repeat('-', 84));
    }
    $prev_size = strpos($r['label'], 'small') !== false ? 'small' : 'big';

    $per_us = $r['per'] / 1000.0;
    $per_fmt = $per_us < 10
        ? sprintf("%.2f µs", $per_us)
        : sprintf("%.1f µs", $per_us);

    printf("%-32s | %-10d | %-14s | %-17s\n",
        $r['label'],
        $r['iter'],
        $per_fmt,
        number_format($r['qps'], 0, '.', ' ') . ' /s');
}
echo "\n";
