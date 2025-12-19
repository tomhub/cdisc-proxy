package main

import (
    "context"
    "database/sql"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "os/signal"
    "strings"
    "sync"
    "syscall"
    "time"
    "strconv"
    "bytes"
    "hash/fnv"

    "github.com/dgraph-io/badger/v4"
    "github.com/goccy/go-yaml"
    _ "github.com/lib/pq"
    "github.com/valkey-io/valkey-go"
    "golang.org/x/sync/singleflight"
)

const (
    maxResponseSize    = 100 << 20 // 100 MB hard safety cap
    maxCacheSize       = 10 << 20  // 10 MB max cached object
    requestTimeout     = 30 * time.Second
    l1PromoteTimeout   = 2 * time.Second  // Reduced from 5s
    defaultGCInterval  = 5 * time.Minute
    shutdownTimeout    = 10 * time.Second
    maxConcurrentReqs  = 100 // Limit concurrent L2 operations
    healthL1TTL        = 1 * time.Minute  // L1 health check to live in L1
    negativeCacheTTL   = 5 * time.Minute // for CDISC replies != 200
    maxPathLengthBeforeHashing      = 512 // Maximum path length
    maxL1ObjectSize    = 2*1024*1024 // 2 Megabytes
)
// based on /mdr/lastupdated vs /mdr/products
var domainToNamespaces = map[string][]string{
    "data-analysis":   {"adam"},
    "data-tabulation": {"sdtm", "sdtmig", "sendig"},
    "data-collection": {"cdash", "cdashig"},
    "terminology":     {"ct"},
    "qrs":             {"qrs"},
    "integrated":      {"integrated"},
}

// --- Config ---

type Config struct {
    Server struct {
        Port    int      `yaml:"port"`
        Listen  []string `yaml:"listen"`
        AuthKey string   `yaml:"auth_key"`
    } `yaml:"server"`
    CDISC struct {
        BaseURL string `yaml:"base_url"`
        APIKey  string `yaml:"api_key"`
    } `yaml:"cdisc"`
    Cache struct {
        L1 struct {
            Driver  string `yaml:"driver"`
            Address string `yaml:"address"`
            TTL     string `yaml:"ttl"`
        } `yaml:"l1"`
        L2 struct {
            BadgerPath      string `yaml:"badger_path"`
            PostgresDSN     string `yaml:"postgres_dsn"`
            TTL             string `yaml:"ttl"`
            CleanupEnabled  bool   `yaml:"cleanup_enabled"`
            CleanupInterval string `yaml:"cleanup_interval"`
        } `yaml:"l2"`
    } `yaml:"cache"`
    Scheduler struct {
        Enabled  bool   `yaml:"enabled"`
        Interval string `yaml:"interval"`
    } `yaml:"scheduler"`
}

// --- Storage Interface ---

// StorageAdapter:
// - Postgres uses product_group as a column
// - Badger encodes product_group (namespace) in the cache key
type StorageAdapter interface {
    UpsertCache(key string, data []byte, expiresAt time.Time, group string) error
    DeleteByProductGroup(group string) error
    CleanupExpired() (int64, error)
    GetCache(key string) ([]byte, time.Time, error)
    GetSystemMeta(key string) (string, error)
    UpsertSystemMeta(key, val string) error
    Ping() error
    Close() error
}

type CachedResponse struct {
    StatusCode int    `json:"s"`
    Body       []byte `json:"b"`
}

type HealthStatus struct {
    Status    string            `json:"status"`
    Timestamp time.Time         `json:"timestamp"`
    Details   map[string]string `json:"details"`
}

const healthCacheKey = "system:health:v1"


type healthMetrics struct {
    mu            sync.RWMutex
    cdiscFailures int
    lastCDISCFail time.Time
}

// --- BadgerDB Adapter ---

type BadgerAdapter struct {
    db *badger.DB
}


func NewBadgerAdapter(path string) (*BadgerAdapter, error) {
    opts := badger.DefaultOptions(path).
        WithLogger(nil).
        WithCompactL0OnClose(true).
        WithNumVersionsToKeep(1). // Keep only latest version
        WithNumLevelZeroTables(2).
        WithNumLevelZeroTablesStall(3)

    db, err := badger.Open(opts)
    if err != nil {
        return nil, err
    }

    return &BadgerAdapter{db: db}, nil
}

func (b *BadgerAdapter) UpsertCache(key string, data []byte, expiresAt time.Time, group string) error {
    // group intentionally unused: namespace is encoded in key
    ttl := time.Until(expiresAt)
    if ttl <= 0 {
        return fmt.Errorf("expiration in past")
    }

    return b.db.Update(func(txn *badger.Txn) error {
        // Badger automatically handles large values via Value Log.
        // For 100MB entries, we ensure we don't block the txn for too long.
        e := badger.NewEntry([]byte(key), data).
            WithTTL(ttl).
            WithMeta(0) // could use Meta to flag 'Large Object' if needed
        return txn.SetEntry(e)
    })
}


func (b *BadgerAdapter) GetCache(key string) ([]byte, time.Time, error) {
    var data []byte
    var expiresAt time.Time

    err := b.db.View(func(txn *badger.Txn) error {
        item, err := txn.Get([]byte(key))
        if err != nil {
            return err
        }

        if ts := item.ExpiresAt(); ts > 0 {
            expiresAt = time.Unix(int64(ts), 0)
        } else {
            expiresAt = time.Now().Add(100 * 365 * 24 * time.Hour) // effectively no expiry
        }
        
        data, err = item.ValueCopy(nil)
        return err
    })

    if err != nil {
        return nil, time.Time{}, err
    }

    return data, expiresAt, nil
}

func (b *BadgerAdapter) DeleteByProductGroup(group string) error {
    // Product group == namespace; encoded in key prefix for Badger
    return b.db.Update(func(txn *badger.Txn) error {
        opts := badger.DefaultIteratorOptions
        opts.PrefetchValues = false
        opts.PrefetchSize = 0

        it := txn.NewIterator(opts)
        defer it.Close()

        prefix := []byte("cdisc:cache:" + strings.ToLower(group) + ":")

        for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
            if err := txn.Delete(it.Item().Key()); err != nil {
                return err
            }
        }
        return nil
    })
}

func (b *BadgerAdapter) CleanupExpired() (int64, error) {
    // TTL is enforced automatically by Badger.
    // Space reclamation is handled via RunValueLogGC.
    return 0, nil
}


func (b *BadgerAdapter) GetSystemMeta(key string) (string, error) {
    var val []byte
    err := b.db.View(func(txn *badger.Txn) error {
        item, err := txn.Get([]byte("system:" + key))
        if err != nil {
            return err
        }
        val, err = item.ValueCopy(nil)
        return err
    })
    if err == badger.ErrKeyNotFound {
        return "", sql.ErrNoRows
    }
    return string(val), err
}

func (b *BadgerAdapter) UpsertSystemMeta(key, val string) error {
    return b.db.Update(func(txn *badger.Txn) error {
        return txn.Set([]byte("system:"+key), []byte(val))
    })
}


func (b *BadgerAdapter) Ping() error {
    if b.db == nil { return errors.New("badger db is nil") }
    // Badger doesn't have a native Ping; we verify by attempting a trivial read
    return b.db.View(func(txn *badger.Txn) error { return nil })
}

func (b *BadgerAdapter) Close() error {
    return b.db.Close()
}

// --- PostgreSQL Adapter ---

type PostgresAdapter struct {
    db *sql.DB
}

func NewPostgresAdapter(dsn string) (*PostgresAdapter, error) {
    db, err := sql.Open("postgres", dsn)
    if err != nil {
        return nil, err
    }

    // Connection pooling
    db.SetMaxOpenConns(25)
    db.SetMaxIdleConns(5)
    db.SetConnMaxLifetime(5 * time.Minute)
    db.SetConnMaxIdleTime(2 * time.Minute)// Close idle conns after 2minutes

    if err := db.Ping(); err != nil {
        db.Close()
        return nil, err
    }

    return &PostgresAdapter{db: db}, nil
}

func (p *PostgresAdapter) UpsertCache(key string, data []byte, expiresAt time.Time, group string) error {
    _, err := p.db.Exec(`
        INSERT INTO cdisc_cache (key, data, expires_at, product_group) 
        VALUES ($1, $2, $3, $4) 
        ON CONFLICT (key) DO UPDATE 
        SET data=$2, expires_at=$3, product_group=$4`,
        key, data, expiresAt, group)
    return err
}

func (p *PostgresAdapter) GetCache(key string) ([]byte, time.Time, error) {
    var data []byte
    var expiresAt time.Time
    err := p.db.QueryRow(
        "SELECT data, expires_at FROM cdisc_cache WHERE key=$1 AND expires_at > $2",
        key, time.Now(),
    ).Scan(&data, &expiresAt)
    return data, expiresAt, err
}

func (p *PostgresAdapter) DeleteByProductGroup(group string) error {
    _, err := p.db.Exec("DELETE FROM cdisc_cache WHERE product_group = $1", group)
    return err
}

func (p *PostgresAdapter) CleanupExpired() (int64, error) {
    result, err := p.db.Exec("DELETE FROM cdisc_cache WHERE expires_at < $1", time.Now())
    if err != nil {
        return 0, err
    }
    return result.RowsAffected()
}

func (p *PostgresAdapter) GetSystemMeta(key string) (string, error) {
    var val string
    err := p.db.QueryRow("SELECT val FROM system_meta WHERE key=$1", key).Scan(&val)
    return val, err
}

func (p *PostgresAdapter) UpsertSystemMeta(key, val string) error {
    _, err := p.db.Exec(`
        INSERT INTO system_meta (key, val) VALUES ($1, $2) 
        ON CONFLICT (key) DO UPDATE SET val=$2`, key, val)
    return err
}

func (p *PostgresAdapter) Ping() error {
    return p.db.Ping()
}

func (p *PostgresAdapter) Close() error {
    return p.db.Close()
}

// helpers
func identifyNamespace(path string) (string, bool) {
    if !strings.HasPrefix(path, "/mdr/") {
        return "", false
    }

    parts := strings.Split(strings.TrimPrefix(path, "/mdr/"), "/")
    if len(parts) == 0 {
        return "", false
    }

    ns := parts[0]

    switch ns {
    case "adam",
        "sdtm",
        "sdtmig",
        "sendig",
        "cdash",
        "cdashig",
        "qrs",
        "ct",
        "integrated":
        return ns, true
    default:
        return "other-ns", false
    }
}

type countingWriter struct {
    w     io.Writer
    limit int64
    count *int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
    if *cw.count >= cw.limit {
        // Nothing written because we've already reached the limit.
        return len(p), nil // pretend we consumed it
    }
    
    remaining := cw.limit - *cw.count
    if int64(len(p)) > remaining {
        p = p[:remaining]
    }
    n, err := cw.w.Write(p)
    *cw.count += int64(n)
    return n, err
}

var copyBufPool = sync.Pool{
    New: func() any {
        b := make([]byte, 32*1024) // 32KB is ideal
        return &b
    },
}

var bufferPool = sync.Pool{
    New: func() any {
        b := make([]byte, 0, 32*1024) // 32KB initial capacity
        return bytes.NewBuffer(b)
    },
}

func (ps *ProxyServer) fetchAndStream(w http.ResponseWriter, r *http.Request, path, key, ns string) error {
    url := ps.config.CDISC.BaseURL + path
    req, _ := http.NewRequestWithContext(r.Context(), "GET", url, nil)
    req.Header.Set("Api-Key", ps.config.CDISC.APIKey)
    req.Header.Set("Accept", "application/json")

    resp, err := ps.httpClient.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.ContentLength > maxResponseSize && resp.ContentLength != -1 {
        http.Error(w, "Response too large", http.StatusBadGateway)
        return nil
    }


    // Memory limit for buffering
    const maxBufferSize = 10 * 1024 * 1024 // 10MB

    // Set response headers BEFORE streaming
    w.Header().Set("Content-Type", "application/json")
    w.Header().Set("X-Cache-Tier", "MISS-STREAM")
    w.WriteHeader(resp.StatusCode)

    buf := bufferPool.Get().(*bytes.Buffer)
    buf.Reset()
    defer bufferPool.Put(buf)
    var capturedBytes int64

    // TeeReader: stream to client while capturing up to maxBufferSize
    tee := io.TeeReader(
        io.LimitReader(resp.Body, maxResponseSize),
        &countingWriter{w: buf, limit: maxBufferSize, count: &capturedBytes},
    )

    bufp := copyBufPool.Get().(*[]byte)
    defer copyBufPool.Put(bufp)

    // Stream to client
    _, err = io.CopyBuffer(w, tee, *bufp)
    if err != nil {
        if errors.Is(err, context.Canceled) || errors.Is(err, syscall.EPIPE) {
            // Client went away — do NOT cache
            return nil
        }
        return err
    }

    // Now we have up to 10MB captured in buf
    capturedData := buf.Bytes()
    
    if len(capturedData) > 0 {

        // Do NOT cache partial responses
        if capturedBytes == maxBufferSize {
            // hit capture limit → likely partial
            return nil
        }
                
        ttl := ps.l2TTL
        l1TTL := ps.l1TTL

        // Only cache 4xx client errors, not 5xx server errors
        if resp.StatusCode >= 400 && resp.StatusCode < 500 {
            ttl = negativeCacheTTL
            l1TTL = negativeCacheTTL
        } else if resp.StatusCode >= 500 {
            // Don't cache server errors
            log.Printf("[Cache] Skipping cache for server error %d on %s", resp.StatusCode, path)
            return nil
        }

        expiry := time.Now().Add(ttl)

        // Async save to L2
        ps.l2Sem <- struct{}{}
        go func(data []byte, status int) {
            defer func() { <-ps.l2Sem }()

            payload, _ := json.Marshal(CachedResponse{
                StatusCode: status,
                Body:       data,
            })

            if err := ps.storage.UpsertCache(key, payload, expiry, ns); err != nil {
                log.Printf("[L2] Cache write failed for %s: %v", key, err)
            }
        }(capturedData, resp.StatusCode)

        // Async save to L1 if small enough
        if len(capturedData) < maxL1ObjectSize {
            ps.asyncSaveToL1(
                key,
                CachedResponse{
                    StatusCode: resp.StatusCode,
                    Body:       capturedData,
                },
                l1TTL,
            )
        } else {
            log.Printf("[L1] Skipping large object (%d bytes) for %s", len(capturedData), key)
        }
    }
    return nil
}

// --- Proxy Server ---

type ProxyServer struct {
    config     Config
    l1Client   valkey.Client
    storage    StorageAdapter
    httpClient *http.Client
    l1TTL      time.Duration
    l2TTL      time.Duration
    ctx        context.Context
    cancel     context.CancelFunc
    wg         sync.WaitGroup
    l2Sem      chan struct{} // Semaphore for L2 operations
    health     *healthMetrics
    sf		   singleflight.Group
}

func NewProxyServer(ctx context.Context, cfg Config) (*ProxyServer, error) {
    if err := validateConfig(cfg); err != nil {
        return nil, fmt.Errorf("invalid config: %w", err)
    }

    l1TTL, err := parseTTL(cfg.Cache.L1.TTL)
    if err != nil {
        return nil, fmt.Errorf("invalid L1 TTL: %w", err)
    }
    l2TTL, err := parseTTL(cfg.Cache.L2.TTL)
    if err != nil {
        return nil, fmt.Errorf("invalid L2 TTL: %w", err)
    }

    serverCtx, cancel := context.WithCancel(ctx)

    ps := &ProxyServer{
        config: cfg,
        httpClient: &http.Client{
            Timeout: requestTimeout,
            Transport: &http.Transport{
                MaxIdleConns:        200,
                MaxIdleConnsPerHost: 20,
                IdleConnTimeout:     90 * time.Second,
                ExpectContinueTimeout: 1 * time.Second,
            },
        },
        l1TTL: l1TTL,
        l2TTL: l2TTL,
        ctx:   serverCtx,
        cancel: cancel,
        l2Sem: make(chan struct{}, maxConcurrentReqs),
        health: &healthMetrics{},
    }

    // Init L1
    l1Client, err := valkey.NewClient(valkey.ClientOption{
        InitAddress: []string{cfg.Cache.L1.Address},
    })
    if err != nil {
        cancel()
        return nil, fmt.Errorf("L1 init failed: %w", err)
    }
    ps.l1Client = l1Client

    // Init L2
    var storage StorageAdapter
    if cfg.Cache.L2.BadgerPath != "" {
        storage, err = NewBadgerAdapter(cfg.Cache.L2.BadgerPath)
        if err != nil {
            cancel()
            l1Client.Close()
            return nil, fmt.Errorf("Badger init failed: %w", err)
        }

        // Start GC
        ps.wg.Add(1)
        go ps.runBadgerGC(storage.(*BadgerAdapter))
    } else {
        storage, err = NewPostgresAdapter(cfg.Cache.L2.PostgresDSN)
        if err != nil {
            cancel()
            l1Client.Close()
            return nil, fmt.Errorf("Postgres init failed: %w", err)
        }
    }

    ps.storage = storage

    // Start background tasks
    if cfg.Scheduler.Enabled {
        ps.wg.Add(1)
        go ps.runScheduler()
    }

    if cfg.Cache.L2.CleanupEnabled {
        ps.wg.Add(1)
        go ps.runCleanup()
    }

    return ps, nil
}

func (ps *ProxyServer) runBadgerGC(adapter *BadgerAdapter) {
    defer ps.wg.Done()
    ticker := time.NewTicker(defaultGCInterval)
    defer ticker.Stop()

    for {
        select {
        case <-ps.ctx.Done():
            return
        case <-ticker.C:
            if err := adapter.db.RunValueLogGC(0.7); err != nil && err != badger.ErrNoRewrite {
                log.Printf("[GC] Error: %v", err)
            }
        }
    }
}

func (ps *ProxyServer) Close() error {
    ps.cancel()
    
    // Wait with timeout
    done := make(chan struct{})
    go func() {
        ps.wg.Wait()
        close(done)
    }()

    select {
    case <-done:
    case <-time.After(shutdownTimeout):
        log.Println("Shutdown timeout exceeded")
    }

    var errs []error
    if ps.l1Client != nil {
        ps.l1Client.Close()
    }
    if ps.storage != nil {
        if err := ps.storage.Close(); err != nil {
            errs = append(errs, err)
        }
    }

    if len(errs) > 0 {
        return fmt.Errorf("close errors: %v", errs)
    }
    return nil
}


func buildCacheKey(r *http.Request, ns string) string {
    uri := r.URL.RequestURI()
    if len(uri) > maxPathLengthBeforeHashing {
        return "cdisc:cache:" + ns + ":" + strconv.FormatUint(fnvHash(uri), 16)
    }
    return "cdisc:cache:" + ns + ":" + uri
}


func fnvHash(s string) uint64 {
    h := fnv.New64a()
    h.Write([]byte(s))
    return h.Sum64()
}

var cachedRespPool = sync.Pool{
    New: func() any { return new(CachedResponse) },
}

// --- HTTP Handlers ---


func (ps *ProxyServer) handleProxy(w http.ResponseWriter, r *http.Request) {
    path := strings.TrimPrefix(r.URL.Path, "/api")
    ns, _ := identifyNamespace(path)
    cacheKey := buildCacheKey(r, ns)



    // 1. Tier 1: L1
    if cached, ok := ps.getFromL1(r.Context(), cacheKey); ok {
        ps.writeResponse(w, cached, "L1-HIT")
        return
    }

    // 2. Tier 2: L2
    if data, _, err := ps.storage.GetCache(cacheKey); err == nil {
        cr := cachedRespPool.Get().(*CachedResponse)
        defer cachedRespPool.Put(cr)

        if json.Unmarshal(data, cr) == nil {
            ps.writeResponse(w, *cr, "L2-HIT")
            return
        }
    }

    // 3. Tier 3: Singleflight upstream
    _, err, shared := ps.sf.Do(cacheKey, func() (interface{}, error) {
        // LEADER ONLY
        return nil, ps.fetchAndStream(w, r, path, cacheKey, ns)
    })

    if err != nil {
        http.Error(w, `{"error":"Upstream Error"}`, http.StatusBadGateway)
        return
    }

    if !shared {
        // Leader already streamed the response
        return
    }

    // FOLLOWERS: wait for cache materialization
    deadline := time.Now().Add(500 * time.Millisecond)
    backoff := 5 * time.Millisecond
    for time.Now().Before(deadline) {
        if cached, ok := ps.getFromL1(r.Context(), cacheKey); ok {
            ps.writeResponse(w, cached, "L1-HIT-FOLLOW")
            return
        }

        if data, _, err := ps.storage.GetCache(cacheKey); err == nil {
            cr := cachedRespPool.Get().(*CachedResponse)
            if json.Unmarshal(data, cr) == nil {
                out := *cr
                cachedRespPool.Put(cr)
                ps.writeResponse(w, out, "L2-HIT-FOLLOW")
                return
            }
            cachedRespPool.Put(cr)
        }
        time.Sleep(backoff)
        if backoff < 50*time.Millisecond {
            backoff *= 2
        }
    }

    return
}


func (ps *ProxyServer) getFromL1(ctx context.Context, key string) (CachedResponse, bool) {
    val, err := ps.l1Client.Do(ctx, ps.l1Client.B().Get().Key(key).Build()).AsBytes()
    if err != nil {
        return CachedResponse{}, false
    }

    cr := cachedRespPool.Get().(*CachedResponse)
    defer cachedRespPool.Put(cr)

    if err := json.Unmarshal(val, cr); err != nil {
        return CachedResponse{}, false
    }

    return *cr, true
}

func (ps *ProxyServer) writeResponse(w http.ResponseWriter, cached CachedResponse, tier string) {
	w.Header().Set("X-Cache-Tier", tier)
	w.Header().Set("Content-Type", "application/json")
	if cached.StatusCode != 200 {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	}
	w.WriteHeader(cached.StatusCode)
	w.Write(cached.Body)
}

func (ps *ProxyServer) fetchFromCDISC(ctx context.Context, path string) ([]byte, int, error) {
	url := ps.config.CDISC.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Api-Key", ps.config.CDISC.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := ps.httpClient.Do(req)
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, 0, err
	}

	return body, resp.StatusCode, nil
}


func (ps *ProxyServer) asyncSaveToL1(key string, cached CachedResponse, ttl time.Duration) {
    cachedBytes, _ := json.Marshal(cached)
    ps.wg.Add(1)
    go func() {
        defer ps.wg.Done()
        ctx, cancel := context.WithTimeout(ps.ctx, l1PromoteTimeout)
        defer cancel()
        if err := ps.l1Client.Do(ctx,
            ps.l1Client.B().Set().Key(key).Value(string(cachedBytes)).Ex(ttl).Build()).Error(); err != nil {
            log.Printf("[L1] async save failed for %s: %v", key, err)
        }
    }()
}

// --- Background Jobs ---

func (ps *ProxyServer) runScheduler() {
    defer ps.wg.Done()
    interval, err := time.ParseDuration(ps.config.Scheduler.Interval)
    if err != nil || interval <= 0 {
        interval = 5 * time.Minute
    }

    ticker := time.NewTicker(interval)
    defer ticker.Stop()

    for {
        select {
        case <-ps.ctx.Done():
            return
        case <-ticker.C:
            if err := ps.checkAndSync(); err != nil {
                log.Printf("[Scheduler] Error: %v", err)
            }
        }
    }
}

func (ps *ProxyServer) invalidateNamespace(ns string) error {
    log.Printf("[Scheduler] Invalidating namespace: %s", ns)
    
    if err := ps.storage.DeleteByProductGroup(ns); err != nil {
        return fmt.Errorf("L2 invalidation failed: %w", err)
    }
    
    if err := ps.invalidateL1ByPrefix("cdisc:cache:" + ns + ":"); err != nil {
        return fmt.Errorf("L1 invalidation failed: %w", err)
    }
    
    return nil
}

func (ps *ProxyServer) invalidateAllNamespaces() {
    namespaces := []string{
        "adam", "sdtm", "sdtmig", "sendig",
        "cdash", "cdashig", "qrs", "ct", "integrated",
    }

    log.Printf("[Scheduler] Global invalidation")

    for _, ns := range namespaces {
        ps.invalidateNamespace(ns)
    }
}


func (ps *ProxyServer) checkAndSync() error {
    ctx, cancel := context.WithTimeout(ps.ctx, 5*time.Second)
    defer cancel()

    body, status, err := ps.fetchFromCDISC(ctx, "/mdr/lastupdated")
    if err != nil || status != http.StatusOK {
        return fmt.Errorf("lastupdated fetch failed: %w", err)
    }

    var remote map[string]string
    if err := json.Unmarshal(body, &remote); err != nil {
        return err
    }

    for domain, newTS := range remote {
        metaKey := "lastupdated:" + domain
        oldTS, _ := ps.storage.GetSystemMeta(metaKey)

        if newTS == oldTS {
            continue
        }

        log.Printf("[Scheduler] %s updated: %s → %s", domain, oldTS, newTS)

        var invalidationErr error
        if domain == "overall" {
            ps.invalidateAllNamespaces()
        } else if namespaces, ok := domainToNamespaces[domain]; ok {
            for _, ns := range namespaces {
                if err := ps.invalidateNamespace(ns); err != nil {
                    log.Printf("[Scheduler] Failed to invalidate %s: %v", ns, err)
                    invalidationErr = err
                }
            }
        }

        // Only update timestamp if invalidation succeeded
        if invalidationErr == nil {
            _ = ps.storage.UpsertSystemMeta(metaKey, newTS)
        }
    }

    return nil
}


func (ps *ProxyServer) invalidateL1ByPrefix(prefix string) error {
    batchSize := 100
    if ps.config.Cache.L1.Driver == "dragonfly" {
        batchSize = 1000
    }

    var cursor uint64
    for {
        select {
        case <-ps.ctx.Done():
            return ps.ctx.Err()
        default:
        }

        res, err := ps.l1Client.Do(ps.ctx,
            ps.l1Client.B().Scan().Cursor(cursor).Match(prefix+"*").Count(int64(batchSize)).Build(),
        ).AsScanEntry()
        if err != nil {
            return err
        }

        if len(res.Elements) > 0 {
            if ps.config.Cache.L1.Driver == "dragonfly" {
                ps.l1Client.Do(ps.ctx, ps.l1Client.B().Del().Key(res.Elements...).Build())
            } else {
                ps.l1Client.Do(ps.ctx, ps.l1Client.B().Unlink().Key(res.Elements...).Build())
            }
        }

        cursor = res.Cursor
        if cursor == 0 {
            break
        }
    }
    return nil
}

func (ps *ProxyServer) runCleanup() {
    defer ps.wg.Done()
    interval, _ := time.ParseDuration(ps.config.Cache.L2.CleanupInterval)
    ticker := time.NewTicker(interval)
    defer ticker.Stop()

    for {
        select {
        case <-ps.ctx.Done():
            return
        case <-ticker.C:
            count, err := ps.storage.CleanupExpired()
            if err != nil {
                log.Printf("[Cleanup] Error: %v", err)
            } else if count > 0 {
                log.Printf("[Cleanup] Removed %d entries", count)
            }
        }
    }
}

// --- Helpers ---

func validateConfig(cfg Config) error {
    if cfg.Server.Port <= 0 || cfg.Server.Port > 65535 {
        return errors.New("invalid port")
    }
    if cfg.CDISC.BaseURL == "" || cfg.CDISC.APIKey == "" {
        return errors.New("CDISC config required")
    }
    if cfg.Cache.L1.Address == "" {
        return errors.New("L1 address required")
    }
    if cfg.Cache.L2.BadgerPath == "" && cfg.Cache.L2.PostgresDSN == "" {
        return errors.New("L2 storage required")
    }
    return nil
}


func parseTTL(ttl string) (time.Duration, error) {
    multipliers := map[string]time.Duration{
        "y": 365 * 24 * time.Hour,
        "w": 7 * 24 * time.Hour,
        "d": 24 * time.Hour,
    }
    
    for suffix, mult := range multipliers {
        if strings.HasSuffix(ttl, suffix) {
            n, err := strconv.Atoi(strings.TrimSuffix(ttl, suffix))
            if err != nil {
                return 0, err
            }
            return mult * time.Duration(n), nil
        }
    }
    
    return time.ParseDuration(ttl)
}


func (ps *ProxyServer) handleHealth(w http.ResponseWriter, r *http.Request) {
    ctx, cancel := context.WithTimeout(r.Context(), 800*time.Millisecond)
    defer cancel()

    w.Header().Set("Content-Type", "application/json")

    // 1. Try to get cached health from L1
    if cached, ok := ps.getFromL1(ctx, healthCacheKey); ok {
        w.Header().Set("X-Health-Source", "L1-Cache")
        ps.writeResponse(w, cached, "HEALTH-CACHE")
        return
    }

    // 2. Perform live checks
    status := HealthStatus{
        Status:    "healthy",
        Timestamp: time.Now(),
        Details:   make(map[string]string),
    }
    isHealthy := true

    // Check L1 (Valkey/Dragonfly)
    testKey := "health:test:"
    if err := ps.l1Client.Do(ctx, ps.l1Client.B().Set().Key(testKey).Value("1").Ex(time.Second).Build(),).Error(); err != nil {
        status.Details["l1"] = "unhealthy: " + err.Error()
        isHealthy = false
    } else {
        status.Details["l1"] = "healthy"
    }

    // Check L2 (Badger/Postgres)
    if err := ps.storage.Ping(); err != nil {
        status.Details["l2"] = "unhealthy: " + err.Error()
        isHealthy = false
    } else {
        status.Details["l2"] = "healthy"
    }

    // Check CDISC Backend (Smallest/Fastest Endpoint)
    // CDISC slow once → health still OK, metric++
    // CDISC slow repeatedly → health degrades
    _, code, err := ps.fetchFromCDISC(ctx, "/mdr/lastupdated")

    ps.health.mu.Lock()
    if err != nil || code != 200 {
        ps.health.cdiscFailures++
        ps.health.lastCDISCFail = time.Now()
    } else {
        ps.health.cdiscFailures = 0
    }
    ps.health.mu.Unlock()

    ps.health.mu.RLock()
    failures := ps.health.cdiscFailures
    lastFail := ps.health.lastCDISCFail
    ps.health.mu.RUnlock()

    if err != nil || code != 200 {
        status.Details["cdisc"] = "slow or unavailable"

        // Only degrade health if it persists
        if failures >= 3 && time.Since(lastFail) < 30*time.Second {
            isHealthy = false
        }
    } else {
        status.Details["cdisc"] = "healthy"
    }


    if !isHealthy {
        status.Status = "degraded"
    }

    // 3. Serialize and Cache in L1 for 1 minute
    respBytes, _ := json.Marshal(status)
    
    // We store it as a CachedResponse so getFromL1 can read it
    // this is healthstatus check reply, not an API service response
    statusCode := http.StatusOK

    healthPayload := CachedResponse{
        StatusCode: statusCode,
        Body:       respBytes,
    }

    ps.asyncSaveToL1(healthCacheKey, healthPayload, healthL1TTL)

    w.Header().Set("X-Health-Source", "Live")
    ps.writeResponse(w, healthPayload, "HEALTH-LIVE")
}

// --- Main ---

func main() {
    cfgFile := "/etc/conf.d/cdisc-proxy.conf"
    if len(os.Args) > 1 {
        cfgFile = os.Args[1]
    }

    data, err := os.ReadFile(cfgFile)
    if err != nil {
        log.Fatalf("Config read failed: %v", err)
    }

    var cfg Config
    if err := yaml.Unmarshal(data, &cfg); err != nil {
        log.Fatalf("Config parse failed: %v", err)
    }

    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()

    server, err := NewProxyServer(ctx, cfg)
    if err != nil {
        log.Fatalf("Server init failed: %v", err)
    }
    defer server.Close()

    // routes
    http.HandleFunc("/api/", server.handleProxy)

    // health
    http.HandleFunc("/health", server.handleHealth)

    listenAddrs := cfg.Server.Listen
    if len(listenAddrs) == 0 {
        listenAddrs = []string{"0.0.0.0"}
    }

    // create servers slice so we can shut them down
    var servers []*http.Server
    for _, addr := range listenAddrs {
        bind := fmt.Sprintf("%s:%d", addr, cfg.Server.Port)
        srv := &http.Server{
            Addr:    bind,
            Handler: nil, // default mux used above
        }
        servers = append(servers, srv)

        go func(s *http.Server) {
            log.Printf("Listening on %s", s.Addr)
            if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
                log.Fatalf("Server error on %s: %v", s.Addr, err)
            }
        }(srv)
    }

    // wait for shutdown signal
    <-ctx.Done()
    log.Println("Shutting down gracefully...")

    // give servers a timeout to finish in-flight requests
    shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
    defer cancel()
    for _, srv := range servers {
        if err := srv.Shutdown(shutdownCtx); err != nil {
            log.Printf("Error shutting down server %s: %v", srv.Addr, err)
        }
    }
}
