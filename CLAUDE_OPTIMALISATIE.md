# CLAUDE_OPTIMALISATIE.md

Methodologie + werklijst voor systematische perf-optimalisatie.

> **Note:** dit bestand is git-ignored (CLAUDE_*.md patroon — zelfde
> als CLAUDE.md). Edits leven lokaal.

## Doel

Stop met "per toeval" allocatie-hotspots vinden. Codebase-breed
mechanisch zoeken naar paden met hoge allocaties, GC-druk, of
contention die we nog niet hebben aangepakt.

## Achtergrond: de les van 2026-05-14

Tijdens de MarshalJSON-perf-sweep ontdekten we per toeval dat
`writeItemsResponse` per /tail-request een 503 KB byte-buffer alloceerde.
Bij 6 kqps saturated betekent dat 3.15 GB/s aan heap-allocatie en
~10-30% van een core aan GC-werk.

**De microbench output toonde 1.22 MB/op AL VOORDAT we deze sessie
begonnen.** Iedere `go test -bench` hangelang stond dat getal op het
scherm, en niemand reageerde. **De methode is niet "vinden" — het
is "lezen wat al daar is".**

Concrete fix: `sync.Pool` voor de buffer, commit 360b079.
Microbench: 725 → 326 µs (2.22× sneller), 1.22 MB → 46 KB per op
(96% reductie). Wrk-saturated: +10% RPS / -10% p50 (queue
gedomineerd, niet meer alloc-gedomineerd).

We hebben in eerdere perf-rondes (Phase 4) deze hotspot gemist
omdat we naar throughput-cijfers keken, niet naar allocaties.
Dit document fixt dat patroon.

## Drie complementaire technieken

### A. `B/op` audit (statisch — eenmalige sweep)

Iedere benchmark die meer dan 100 KB per call alloceert is verdacht.
Soms terecht (bulk-endpoints met grote payloads), soms gemist.

```bash
docker compose exec -T dev sh -c 'cd /src && go test -bench=. -benchmem -run=^$ -benchtime=1s ./...' \
  | awk '/^Benchmark/ { bytes = $5+0; if (bytes > 100000) print }'
```

Een single sweep over alle bench-files. Output is een korte lijst
"verdachte" benches. Voor elke: onderzoek of de allocatie
noodzakelijk is.

### B. `pprof -alloc_space` (dynamisch — onder load)

Toont welke functies cumulatief de meeste bytes hebben gealloceerd —
niet wat in geheugen STAAT, maar wat ooit gealloceerd is. Perfect
voor het vinden van high-rate allocators.

```bash
# Run een bench die saturated load oplegt, capture mem-profile:
docker compose exec -T dev sh -c 'cd /src && \
    go test -bench=BenchmarkHTTP -memprofile=/tmp/mem.prof -benchtime=3s ./...'

# Top callers by cumulative allocated bytes:
docker compose exec -T dev sh -c 'cd /src && \
    go tool pprof -alloc_space -top -cum /tmp/mem.prof' | head -20

# Of grafische weergave (via SVG of HTTP):
docker compose exec -T dev sh -c 'cd /src && \
    go tool pprof -alloc_space -http=:9999 /tmp/mem.prof'
```

Output toont per-functie wie de grote allocators zijn. Soms is dat
ergens diep in een dependency (`encoding/json`), soms in onze
eigen code (`writeItemsResponse` was 1.17 MB/req).

### C. `GODEBUG=gctrace=1` (runtime — tijdens iedere wrk-run)

Elke GC-cycle wordt naar stderr gelogd:

```
gc 142 @1.245s 12%: 0.123+15.4+0.012 ms clock, ...
```

**Het tweede getal (12%) is GC-CPU als percentage van totaal**.
Boven 10% kritisch. Boven 20% dramatisch.

Suggested addition to `scripts/bench.sh`: optionele `--gctrace` flag
die de server-container met `GODEBUG=gctrace=1` start en de GC-stats
in de bench-output toont.

## Per-endpoint allocation budget (concept)

Documenteer voor elke handler een verwachte allocatie-grens.
Devs zien de verwachting; bench detecteert overschrijdingen.

| endpoint | huidige B/op | budget (voorgesteld) |
|---|---|---|
| `/get` (single item) | ~1 KB | 2 KB |
| `/head` (10 items, 512 B) | 8 KB | 16 KB |
| `/tail` (100 items, 5 KiB) | 46 KB (post-pool) | 64 KB |
| `/scopelist` | onbekend — meten | 32 KB |
| `/append` | onbekend — meten | 8 KB |
| `/upsert` / `/update` | onbekend — meten | 8 KB |
| `/counter_add` | onbekend — meten | 4 KB |
| `/delete*` | onbekend — meten | 1 KB |
| `/wipe` | onbekend — meten | 4 KB |
| `/warm` (1000 items) | onbekend — meten | 200 KB |
| `/warm` (10000 items) | onbekend — meten | 1500 KB |
| `/rebuild` (idem) | onbekend — meten | als warm |
| `/stats` | onbekend — meten | 4 KB |

**Eerste stap is meten** — zonder cijfers is een budget gokken.
Daarna budgets in dit document als hard signaal: overschrijdt een
PR het budget → flag.

## Open verdachten

Niet bewezen, alleen vermoedens uit code-inspectie:

1. **`writeGetResponse`** — bouwt prefix + suffix per /get-request
   (~100 B elk). 270k/s × 200 B = ~54 MB/s aan kleine allocaties.
   Eenvoudig poolbaar.
2. **`gateway.Tail()` / `gateway.Head()` return-slices** — kopieert per
   call `[]Item` met N headers. Voor /tail 100 items: 100 × 56 B =
   5.6 KB headers. Pool of slice-pool kan dit naar 0.
3. **`writeAck`-struct in `newWriteAck`** — per write een pointer-alloc
   voor `idPtr` als ID non-empty. ~16 B/req over write-endpoints.
4. **`bytes.Reader` rond request body** — `decodeBody` wraps `r.Body`
   in `MaxBytesReader` per call. Niet zelf alloceren, maar dependency
   doet het wel.
5. **`emitEvent` → `appendOneTrusted` → Item-kopieer-pad** — elke
   write die _events triggert kopieert de Item-struct. Mogelijk poolbaar.
6. **`http.ResponseWriter` interne buffers** — niet onze code maar
   het kost wel allocatie per request. Sommige toolboxes hebben hier
   "responseWriter wrapping" voor optimalisatie.
7. **HTTP header-allocaties** — `w.Header().Set(...)` mut een map.
   Maps doen kleine allocaties. Mogelijk poolbaar via `sync.Pool[Header]`.

Beste eerste stap: techniek A + B draaien, kijken welke van deze
vermoedens wordt bevestigd door pprof. Niet vooruit-optimaliseren
op vermoedens.

## Werkwijze (gestructureerd)

Per ronde:

1. **Meet eerst** — `B/op` audit + pprof onder load. Genereer een
   shortlist van top-3 hotspots.
2. **Kies één** — niet alles tegelijk; per ronde één target.
3. **Schrijf de bench** — als er nog geen Go-bench is voor dit pad,
   maak 'm. Run before-state.
4. **Fix** — sync.Pool, slice-reuse, allocation-hoist out of loop,
   etc.
5. **Run after-state** — bench moet de verbetering tonen.
6. **Saturated wrk verifieren** — als de bench-winst > 30%, run
   `scripts/bench.sh` om end-to-end impact te zien.
7. **Commit** — separate commit per fix met before/after cijfers
   in het commit-message.
8. **Update budgets** — als de fix het endpoint nu binnen budget
   brengt, update dit document.

Tussen rondes: dit document bijwerken met geleerde lessen, nieuwe
verdachten, gemeten waardes.

## Niet doen

- **Niet pre-optimaliseren op vermoedens** zonder eerst meten.
- **Niet allocaties verminderen ten koste van leesbaarheid** zonder
  bench die het waard maakt.
- **Niet B/op verlagen voor cold-paths** (admin endpoints, error
  paths, init). Alleen hot paths verdienen aandacht.
- **Niet pool-everything**. Sync.Pool heeft overhead (per-P caches,
  GC interactie). Pool alleen wanneer de alloc ≥ 1 KB EN frequency ≥
  1 kqps.

## Lessen tot nu toe

- **Microbench output IS data**. Lees B/op naast ns/op. Hoge B/op =
  verdacht, ook als ns/op goed lijkt.
- **Throughput-bench (rps) verbergt allocaties**. Saturated load
  toont CPU + I/O bottlenecks; alloc-pressure pas zichtbaar in p99
  tail (GC-pauses).
- **sync.Pool werkt fantastisch** voor middelgrote buffers (10 KB —
  1 MB) en hoge call-rates. Daaronder: ruis. Daarboven: pin memory.
- **JSON-encoder swap (goccy) is universeel positief** behalve op
  `json.Valid` waar stdlib's byte-scanner sneller is. Altijd meten,
  niet aannemen.
- **MarshalJSON-methodes toevoegen werkt averechts voor kleine
  structs** — de interface-plumbing overhead (~250 ns) is hoger dan
  pure reflectie over 3-5 velden.
