# cdisc-proxy Architecture

This document explains how `cdisc-proxy` works internally, for people who want fewer surprises.

---

## High-level overview

```
        Clients
           |
           v
     +-------------+
     | cdisc-proxy |
     +-------------+
        |     |
        |     +------------------+
        |                        |
        v                        v
   L1 Cache (Redis)        L2 Cache (Durable)
                                 |
                      +-----------------------+
                      | sled + FS | PostgreSQL|
                      +-----------------------+
                                 |
                                 v
                          CDISC MDR API
```

---

## Request flow

1. Client sends `GET /api/mdr/...`
2. Request is validated and canonicalized
3. Cache key is computed deterministically
4. Lookup order:
   - L1 cache (Redis)
   - L2 cache (sled/Postgres)
5. If cache miss:
   - Single-flight leader is elected
   - One upstream request is made
   - Followers wait for result
6. Response is cached:
   - L2 synchronously
   - L1 asynchronously
7. Response returned to all waiting clients

---

## Canonical cache keys

Cache keys are normalized to avoid:
- duplicate slashes
- reordered query parameters
- percent-encoding differences

Format:
```
cdisc:cache:{namespace}:{path}?{sorted_query}
```

This prevents cache fragmentation and accidental DoS-by-variation.

---

## L1 Cache (Redis / Valkey)

Purpose:
- Ultra-fast response for hot objects
- Short TTL
- Best-effort only

Constraints:
- Max object size enforced
- TTL capped by L2 expiration

If Redis dies, the proxy shrugs and continues.

---

## L2 Cache (Durable)

### Option 1: sled + filesystem blobs

- Metadata stored in sled
- Large bodies stored as blobs on disk
- Eviction under disk pressure
- Expired entries cleaned opportunistically

This is the default and recommended option.

### Option 2: PostgreSQL

- All data stored in tables
- Easier to audit
- Slower, but familiar to enterprises

---

## Single-flight protection

Concurrent identical requests are coalesced:

- First request becomes **leader**
- All others wait on a watch channel
- Exactly one upstream request is performed

This prevents cache stampedes and upstream abuse.

Tested at tens of thousands of concurrent requests.

---

## Negative caching

- 2xx / 3xx → normal TTL
- 4xx → cached longer (client error ≠ transient)
- 5xx → cached briefly (upstream instability)

This avoids retry storms caused by broken clients.

---

## Backpressure & limits

- Global upstream semaphore
- Max concurrent requests enforced
- Request timeout applied at HTTP layer

The proxy will slow clients down instead of collapsing.

---

## Health checks

Endpoint: `GET /health`

Checks:
- L1 availability
- L2 availability

Health responses are cached to avoid self-inflicted load.

---

## Failure philosophy

- Redis failure → degraded, still functional
- L2 failure → degraded, still functional
- Upstream failure → cached negativity
- Misconfiguration → fast failure at startup

No heroics. No magic. Just predictable behavior.

---

## Final note

This system assumes:
- upstream data is mostly immutable
- correctness matters more than novelty
- badgers are wiser than raccoons

<div align="center">
🦡 Powered by Badgers. 🦝 Guarded by Racoons. Deployed by Enthusiasts. Made with ❤️ for the clinical research community.
</div>
