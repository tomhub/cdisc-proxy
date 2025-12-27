## cdisc-proxy
An untested caching proxy for the CDISC Library API.

It exists because repeatedly hammering the same upstream endpoint is wasteful, slow, and something only **raccoons with a network cable** would design.  
Badgers reviewed the design. They were unimpressed, but approved it anyway. 🦝🦡

MIT licensed. Use it, fork it, question it.

---

## What it is

`cdisc-proxy` is a **read-only reverse HTTP proxy** with deterministic caching, built specifically for CDISC Library API endpoints.

It sits between your clients and the CDISC API and ensures that:
- identical requests are treated identically
- upstream calls are minimized
- concurrency does not turn into chaos

---

## What it does

- Proxies **GET** requests under `/api/mdr/...`
- Talks to CDISC Library API using an API key
- Canonicalizes requests to avoid cache poisoning
- Uses **two-tier caching**
  - L1: Redis / Valkey (fast)
  - L2: sled+filesystem blobs **or** PostgreSQL (durable)
- Uses **single-flight** request coalescing
- Applies negative caching for errors
- Applies backpressure to upstream calls
- Survives high concurrency without panicking

---

## What it does *not* do

- ❌ Write or mutate upstream data
- ❌ Pretend bad upstream responses are your fault
- ❌ Authenticate users beyond an optional shared key
- ❌ Care about badly written clients

---

## TODO

- Test it
- Break it
- Patch it
- Repeat it

---

## Installation

### Build from source

```bash
git clone https://github.com/your-org/cdisc-proxy.git
cd cdisc-proxy
cargo build --release
```

Binary:
```bash
target/release/cdisc-proxy
```

---

## Alpine Linux

```bash
apk add --no-cache ca-certificates libgcc libstdc++ redis postgresql-client
cp target/release/cdisc-proxy /usr/local/bin/
mkdir -p /etc/conf.d
```

Run:
```bash
cdisc-proxy /etc/conf.d/cdisc-proxy.conf
```

---

## Ubuntu / Debian

```bash
apt update
apt install -y redis postgresql-client ca-certificates
cp target/release/cdisc-proxy /usr/local/bin/
```

Run:
```bash
cdisc-proxy /etc/cdisc-proxy.yaml
```

---

## Docker

```dockerfile
FROM alpine:3.20
RUN apk add --no-cache ca-certificates libgcc libstdc++
COPY cdisc-proxy /usr/local/bin/cdisc-proxy
COPY cdisc-proxy.yaml /etc/cdisc-proxy.yaml
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/cdisc-proxy", "/etc/cdisc-proxy.yaml"]
```

---

## Configuration

Example:

```yaml
server:
  port: 8080
  listen: ["0.0.0.0"]
  auth_key: null

cdisc:
  base_url: "https://api.cdisc.org"
  api_key: "REDACTED"

cache:
  l1:
    driver: "redis"
    address: "redis://127.0.0.1:6379"
    ttl: "10m"

  l2:
    badger_path: "/var/lib/cdisc-proxy"
    postgres_dsn: null
    ttl: "30d"
    cleanup_enabled: true
    cleanup_interval: "1h"

scheduler:
  enabled: false
  interval: "1h"
```

---

## Endpoints

- `GET /api/...` — proxied CDISC Library API calls
- `GET /health` — cached + live health checks

Response header:
```
X-Cache-Tier: L1-HIT | L2-HIT | MISS
```

## License

MIT.  
Do what you want.  
If it breaks, check your config before blaming the badgers.

<div align="center">
🦡 Powered by Badgers. 🦝 Guarded by Racoons. Deployed by Enthusiasts. Made with ❤️ for the clinical research community.
</div>
