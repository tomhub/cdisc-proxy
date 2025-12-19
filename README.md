# 🚀 CDISC Library API Proxy

> A fast, **extremely opinionated**, mildly unhinged (by design) caching proxy for the **CDISC Library API**.
>
> Built in Go. Tuned for production. Designed to survive real-world CDISC outages without flapping, stampeding, or waking you up at 03:00 because *someone reran the pipeline*.
>
> Yes, it caches errors.  
> No, that’s not a bug.  
> Yes, we’ve thought about it more than you have.

---

## 😈 What This Thing Does (And Why You’re Already Late to the Party)

Let’s establish some uncomfortable truths[citation required]:

- You do **not** control the CDISC Library API  
- It will be slow, eventually  
- It will be down, occasionally  
- Your pipelines will react like toddlers on espresso  

So this proxy exists to sit in the middle, arms crossed, and say:

> “Absolutely not. You will not all panic at once.”

### In one breath (because you’re busy):

- **Always caches** (even non-200 responses, briefly, on purpose)
- **Two-tier cache**
  - ⚡ L1 RAM cache (Valkey / Redis / Dragonfly)
  - 🗄️ L2 persistent cache (BadgerDB or PostgreSQL)
- **Singleflight deduplication** — one upstream call, everyone else waits
- **Streaming leader / cached followers**
- **Namespace-aware invalidation** via `/mdr/lastupdated`
- **Health checks that don’t scream** because CDISC sneezed

If you’ve ever watched a CI pipeline accidentally DDoS CDISC…  
congratulations, this proxy is your emotional support mammal.

---

## 🧠 Big Picture (AKA “Where the Panic Is Contained”)

```
Client
  │
  ▼
🧠 CDISC Proxy (this repo)
  │
  ├─ ⚡ L1 Cache (Valkey / Redis / Dragonfly)
  │    └─ tiny, hot, easily offended
  │
  ├─ 🦡 L2 Cache (BadgerDB or PostgreSQL)
  │    └─ durable, grumpy, hoards metadata forever
  │
  └─ 🌐 CDISC Library API
       └─ singleflight protected (no stampedes, riots, or regrets)
```

Think of this as a shock absorber.  
Or a bouncer.  
Or a racoon guarding a dumpster full of cached metadata.

---

## 🏎️ The Life of a Request (No Fairy Tales)

1. **L1 lookup** — hit? instant response.
2. **L2 lookup** — hit? served from disk. Small enough? Copy self to L1.
3. **Upstream call (singleflight)** — one request, many followers.

No thundering herds.  
No duplicate CDISC calls.  
No surprise retrospectives.

---

## 🧊 Cache Keys (Yes, We Thought About This)

```
cdisc:cache:<namespace>:<request-uri>
```

Status codes survive caching:

```json
{ "s": 404, "b": "{ \"error\": \"Not Found\" }" }
```

No fake 200s. No lies.

---

## 😈 Negative Caching

Non-200 responses are cached for 5 minutes:

```go
negativeCacheTTL = 5 * time.Minute
```

Outages become boring.  
Boring is good.

---

## 🧭 Namespace-Aware Invalidation

Uses `/mdr/lastupdated`.

Flush only what changed.  
Not the universe.

---

## 🤡 Why Not Just Use NGINX?

You can.

NGINX is a **dumb fridge**.  
This proxy is a **judgmental racoon**.

NGINX can’t:
- Deduplicate upstream calls
- Invalidate by CDISC namespace
- Avoid stampedes
- Understand CDISC semantics

---

## 🤡 Why Not Varnish?

Varnish is excellent at:
- Being fast
- Being stateless
- Being *your problem at 02:00*

It still:
- Has no idea what SDTM is
- Can’t read `/mdr/lastupdated`
- Will happily serve stale-but-fast lies
- Requires ritual VCL sacrifices

---

## 📖 A True Story (Postmortem Edition, LLM Hallucination) [not true story]

**02:17 UTC**  
CDISC slows down.

**02:18 UTC**  
CI pipelines notice.

**02:19 UTC**  
400 identical requests hit `/mdr/sdtm`.

**02:20 UTC**  
NGINX shrugs.

**02:21 UTC**  
CDISC rate-limits you.

**02:22 UTC**  
Slack explodes.

With this proxy:
- First request goes out
- Others wait
- Cache fills
- Everyone goes back to sleep

---

## 📜 License

MIT. Do what you want. Just don’t pretend you weren’t warned.

---

<div align="center">
🦡 Powered by Badgers. 🦝 Guarded by Racoons. Deployed by Enthusiasts. Made with ❤️ for the clinical research community.
</div>
