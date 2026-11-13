# pair-wise tender crawler

A pure-Go, local-first crawler for consolidating tender announcements from
multiple district portals. It uses Colly for static HTML retrieval, SQLite
(`modernc.org/sqlite`, no CGO) for storage, and deterministic local HTTP
fixtures for the demonstration.

## Quick start

```bash
go mod download
go test ./...
go run ./cmd/crawler demo --pages 600 --sites 3 --db data/crawl.db --report report.json
```

The last command is fully self-contained: it starts local `httptest` servers,
simulates timeouts, refused connections, HTTP 4xx/5xx/429, broken selectors,
GBK pages, forwarding links, revisions and retractions, then runs full,
incremental and re-run phases against one SQLite file.

Expected terminal output:

```text
mode=full fetched=600 raw_url=612 uniq_url=591 dedup=21(3.4%) new=573 revised=17 failed=29(timeout=9,refused=1,http_4xx=11,http_5xx=2,parse=6) watermark=2026-11-12T09:30:00Z|P-0591
mode=incr fetched=140 new=22 revised=3 dedup=115
rerun=new:0 revised:0 rows=593 idempotent=true
```

The same values are stored in `report.json`.

## Production commands

```bash
# First full reconciliation (ignores the watermark, but still deduplicates)
go run ./cmd/crawler run --mode full --config config/sites.yaml \
  --db data/crawl.db --report full.json

# Daily incremental crawl: composite watermark + 30-minute lookback
go run ./cmd/crawler run --mode incr --config config/sites.yaml \
  --db data/crawl.db --report incr.json

# Replay one failure class, for example selector/parse failures
go run ./cmd/crawler replay --class=parse --config config/sites.yaml \
  --db data/crawl.db --report replay.json

# Read the latest run and current database counters
go run ./cmd/crawler stats --db data/crawl.db
```

Configuration is described by `config/sites.yaml`. Selectors are CSS
selectors relative to each item; append `@attr` to read an attribute
(for example `.title@href`). `detail: true` fetches each announcement's
link and applies the same field selectors to the detail document.

## Correctness model

- **Identity**: canonical URL first, business key `(project number, bid
  section)` second. URL aliases retain every forwarding source, while the
  announcement keeps its first source.
- **Revision**: a new content fingerprint for an existing business key
  appends a version row. Historical versions replayed later are duplicates,
  not revisions, so a whole batch can be re-run idempotently.
- **Watermark**: `(publish_time, business_key)` is updated in the same
  transaction as records and version chains. Incremental filtering uses the
  composite cursor plus a 30-minute overlap window. Full runs ignore the
  cursor and recompute it from the database after the batch.
- **Atomicity**: records, aliases, versions and the watermark are one SQLite
  transaction. Cancellation or a failed batch rolls everything back.
- **Failures**: `timeout`, `refused`, `http_4xx`, `http_5xx`, `rate_limited`,
  `parse`, and `network` are persisted with site, URL, kind and HTTP status
  for class-specific replay.
- **Content**: UTF-8/GBK detection, NFKC/whitespace title normalization,
  relative and protocol-relative URL resolution, body truncation, empty-title
  rejection, and pagination guards for missing next links, self links,
  revisits and non-increasing page numbers.
- **Rows**: `rows` counts active announcements. A retracted (withdrawn)
  revision remains in the version chain but is excluded from the active count.
- **Concurrency/limits**: each site has a token bucket, a configurable
  concurrent-request limit and a consecutive-5xx circuit breaker. No browser
  or script execution is used.

The demo is designed to finish in a few seconds and keeps the SQLite file well
below 50 MB.
