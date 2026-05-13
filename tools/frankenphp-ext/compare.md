# PHP get cost — Redis vs Memcached vs scopecache (cgo)

Resultaten van [`compare.sh`](compare.sh) + [`compare.php`](compare.php).
Host-machine, Docker bridge network, FrankenPHP 1.12 / PHP 8 ZTS,
phpredis + ext-memcached uit `install-php-extensions`.

## Resultaat

| pad | klein (54 B) | groot (5 KiB) |
|---|---|---|
| Redis cold (connect + get + close per call) | 607 µs | 607 µs |
| Redis warm (persistent connection) | 130 µs | 127 µs |
| Memcached cold (new client + addServer per call) | 640 µs | 635 µs |
| Memcached warm (persistent client) | 127 µs | 127 µs |
| **scopecache cgo (in-process)** | **0.70 µs** | **4.44 µs** |

## Wat opvalt

1. **TCP-setup kost ~480 µs.** Het verschil tussen cold en warm. Bij
   elke "nieuwe connectie per call" (de naïeve PHP-stijl zonder
   pconnect) gaat ~80% van de tijd op aan handshake, niet aan de
   cache zelf.

2. **Redis en Memcached liggen vrijwel gelijk** (~127 µs warm). De
   netwerk-roundtrip over de Docker-bridge domineert; de cache-engine
   zelf is verwaarloosbaar in vergelijking.

3. **Payload-grootte raakt Redis/Memcached nauwelijks.** 127 → 127 µs
   van 54 B naar 5 KiB, omdat de TCP-roundtrip de bottleneck is, niet
   de bytes. scopecache schaalt wél met payload (0.70 → 4.44 µs) want
   daar zit de JSON-decode kost van de extension.

4. **scopecache vs Redis warm:**
   - 54 B  →  186× sneller (130 µs / 0.70 µs)
   - 5 KiB →   29× sneller (127 µs / 4.44 µs)

   Geen connectiebeheer, geen TCP, geen wire-serialisatie — de hele
   call gaat via één cgo-trampoline naar het in-process `*Gateway`.

## Hoeveel daarvan is Docker-bridge?

Eenmalige controlemeting: PHP + Redis + Memcached in **één** container,
communicerend via `127.0.0.1` (geen Docker-bridge tussen processen).
Tijdelijk script `compare-localhost.sh` (kan weer weg na deze meting).

| pad | bridge (compare.sh) | localhost (1 container) | bridge-overhead |
|---|---|---|---|
| Redis cold 54 B | 607 µs | 178 µs | -429 µs |
| Redis warm 54 B | 130 µs | 106 µs | **-24 µs** |
| Redis warm 5 KiB | 127 µs | 103 µs | **-24 µs** |
| Memcached cold 54 B | 640 µs | 256 µs | -384 µs |
| Memcached warm 54 B | 127 µs | 113 µs | **-14 µs** |
| Memcached warm 5 KiB | 127 µs | 97 µs | **-30 µs** |
| scopecache cgo 54 B | 0.70 µs | 0.72 µs | 0 (geen TCP) |
| scopecache cgo 5 KiB | 4.44 µs | 4.12 µs | 0 (geen TCP) |

Lezing:

1. **Bridge kost ~14-30 µs per warm-call** op deze WSL2-host. Minder
   dan ik op het oog schatte.
2. **Cold-path verschil is veel groter** (~430 µs Redis). Een
   TCP-handshake over de bridge is duurder dan over loopback —
   meerdere extra netfilter-stappen per pakket.
3. **scopecache verandert niet** (0.70 ↔ 0.72 µs). Verwacht: cgo zit
   niet op een socket, de Docker-laag is irrelevant.
4. **scopecache vs Redis warm in de gunstigste opstelling voor Redis:**
   nog steeds 147× verschil (106 µs / 0.72 µs). Twee orders of
   magnitude blijft staan.

Conclusie: de 127 µs in `compare.sh` is representatief voor productie.
De Redis-engine + phpredis zelf zit op ~100 µs, de overige ~25 µs is
Docker-bridge — geen van beide is misleidend.

## Methodologie

Drie backends in één Docker-network:

- `redis:7-alpine`
- `memcached:1.6-alpine`
- FrankenPHP-binary met scopecache als Caddy-module + de cgo
  extension ingebakken (`tools/frankenphp-ext/dist/frankenphp`).

Elke backend wordt aan het begin geseed met dezelfde twee payloads
(54 B en 5 KiB, beide JSON-strings). Daarna:

- 1000 warmup-iters per pad,
- 2000 iters voor de cold-paden (TCP-setup × 2000 = ~1.2 s wallclock),
- 20000 iters voor warm Redis/Memcached,
- 100000 iters voor scopecache (cgo is goedkoop genoeg dat hogere
  iters nodig zijn voor stabiele timing).

`hrtime(true)` om de loop. Geen tail-trim, geen median — gewoon de
gemiddelde per-call kost over de hele run.

## Re-run

```bash
cd tools/frankenphp-ext
./compare.sh
```

Per-knob overrides via env-vars:

```bash
ITER_COLD=5000 ITER_WARM=50000 ITER_LOCAL=200000 ./compare.sh
```

Eerste run bouwt het runtime-image `frankenphp-ext-bench:local`
eenmalig (~30-60 s); daarna direct door naar de bench.
