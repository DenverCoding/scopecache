# PHP get cost — Redis vs Memcached vs scopecache (cgo)

Results from [`compare.sh`](compare.sh) + [`compare.php`](compare.php).
Host machine, Docker bridge network, FrankenPHP 1.12 / PHP 8 ZTS,
phpredis + ext-memcached installed via `install-php-extensions`.

## Result

| path | small (54 B) | large (5 KiB) |
|---|---|---|
| Redis cold (connect + get + close per call) | 607 µs | 607 µs |
| Redis warm (persistent connection) | 130 µs | 127 µs |
| Memcached cold (new client + addServer per call) | 640 µs | 635 µs |
| Memcached warm (persistent client) | 127 µs | 127 µs |
| **scopecache cgo (in-process)** | **0.70 µs** | **4.44 µs** |

## Observations

1. **TCP setup costs ~480 µs.** The gap between cold and warm. In the
   naive PHP style without `pconnect`, ~80% of the time goes to the
   handshake — not to the cache itself.

2. **Redis and Memcached land close to each other** (~127 µs warm).
   The network round-trip over the Docker bridge dominates; the
   cache engine itself is negligible in comparison.

3. **Payload size barely affects Redis/Memcached.** 127 → 127 µs
   from 54 B to 5 KiB because the TCP round-trip is the bottleneck,
   not the bytes. scopecache does scale with payload (0.70 → 4.44 µs)
   because that path includes the extension's JSON-decode cost.

4. **scopecache vs Redis warm:**
   - 54 B  →  186× faster (130 µs / 0.70 µs)
   - 5 KiB →   29× faster (127 µs / 4.44 µs)

   No connection management, no TCP, no wire serialization — the
   entire call goes through one cgo trampoline into the in-process
   `*Gateway`.

## How much of that is the Docker bridge?

One-off control run: PHP + Redis + Memcached in **one** container,
talking via `127.0.0.1` (no Docker bridge between processes).
Throwaway script `compare-localhost.sh` (removed once the result was
recorded).

| path | bridge (compare.sh) | localhost (1 container) | bridge overhead |
|---|---|---|---|
| Redis cold 54 B | 607 µs | 178 µs | -429 µs |
| Redis warm 54 B | 130 µs | 106 µs | **-24 µs** |
| Redis warm 5 KiB | 127 µs | 103 µs | **-24 µs** |
| Memcached cold 54 B | 640 µs | 256 µs | -384 µs |
| Memcached warm 54 B | 127 µs | 113 µs | **-14 µs** |
| Memcached warm 5 KiB | 127 µs | 97 µs | **-30 µs** |
| scopecache cgo 54 B | 0.70 µs | 0.72 µs | 0 (no TCP) |
| scopecache cgo 5 KiB | 4.44 µs | 4.12 µs | 0 (no TCP) |

Reading:

1. **The bridge costs ~14-30 µs per warm call** on this WSL2 host.
   Lower than a rough estimate would suggest.
2. **The cold-path difference is much larger** (~430 µs for Redis).
   A TCP handshake over the bridge is more expensive than over
   loopback — several extra netfilter steps per packet.
3. **scopecache does not move** (0.70 ↔ 0.72 µs). Expected: cgo is
   not on a socket, so the Docker layer is irrelevant.
4. **scopecache vs Redis warm in the most Redis-favorable setup:**
   still a 147× gap (106 µs / 0.72 µs). Two orders of magnitude
   remain.

Conclusion: the 127 µs in `compare.sh` is representative for
production. The Redis engine + phpredis themselves sit around 100 µs;
the remaining ~25 µs is Docker bridge — neither number is misleading.

## Methodology

Three backends on one Docker network:

- `redis:7-alpine`
- `memcached:1.6-alpine`
- FrankenPHP binary with scopecache compiled in as both the Caddy
  module and the cgo extension (`tools/frankenphp-ext/dist/frankenphp`).

Each backend is seeded with the same two payloads at start
(54 B and 5 KiB, both JSON-strings). Then:

- 1000 warmup iters per path,
- 2000 iters for the cold paths (TCP setup × 2000 ≈ 1.2 s wallclock),
- 20000 iters for warm Redis/Memcached,
- 100000 iters for scopecache (cgo is cheap enough that higher iter
  counts are needed for stable timings).

`hrtime(true)` around the loop. No tail trim, no median — just the
average per-call cost across the whole run.

## Re-run

```bash
cd tools/frankenphp-ext
./compare.sh
```

Per-knob overrides via env vars:

```bash
ITER_COLD=5000 ITER_WARM=50000 ITER_LOCAL=200000 ./compare.sh
```

The first run builds the runtime image `frankenphp-ext-bench:local`
once (~30-60 s); subsequent runs reuse it.
