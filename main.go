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
	"strings"
	"sync"
	"time"
	"strconv"

	"github.com/goccy/go-yaml"
	_ "github.com/lib/pq"
	"github.com/valkey-io/valkey-go"
	"github.com/dgraph-io/badger/v4"
)

const (
	maxResponseSize = 100 * 1024 * 1024 // 100MB limit
	requestTimeout  = 30 * time.Second
)

// --- Structs for API & Cache ---

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

type ProxyServer struct {
	config     Config
	l1Client   valkey.Client
	l2Db       *sql.DB
	storage    StorageAdapter
	httpClient *http.Client
	l1TTL      time.Duration
	l2TTL      time.Duration
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	mu         sync.RWMutex // Protects concurrent cache operations
}

// StorageAdapter abstracts DB-specific operations
type StorageAdapter interface {
    UpsertCache(key string, data []byte, expiresAt time.Time, group string) error
    UpsertSystemMeta(key, val string) error
    UpsertProductMeta(id, ts string) error
    DeleteByProductGroup(group string) error
    CleanupExpired() (int64, error)
    GetCache(key string) ([]byte, time.Time, error) 
}

type CachedResponse struct {
	StatusCode int    `json:"status_code"`
	Body       []byte `json:"body"`
}


// --- Storage Implementations ---
type BadgerAdapter struct {
    db *badger.DB
}

type cacheEntry struct {
    Data      []byte
    ExpiresAt time.Time
    Group     string
}

func (b *BadgerAdapter) UpsertCache(key string, data []byte, expiresAt time.Time, group string) error {
    entry := cacheEntry{Data: data, ExpiresAt: expiresAt, Group: group}
    val, err := json.Marshal(entry)
    if err != nil {
        return err
    }
    return b.db.Update(func(txn *badger.Txn) error {
        return txn.Set([]byte(key), val)
    })
}

func (b *BadgerAdapter) UpsertSystemMeta(key, val string) error {
    return b.db.Update(func(txn *badger.Txn) error {
        return txn.Set([]byte("system:"+key), []byte(val))
    })
}

func (b *BadgerAdapter) UpsertProductMeta(id, ts string) error {
    return b.db.Update(func(txn *badger.Txn) error {
        return txn.Set([]byte("product:"+id), []byte(ts))
    })
}

func (b *BadgerAdapter) DeleteByProductGroup(group string) error {
    // Scan all keys and delete those with matching group
    return b.db.Update(func(txn *badger.Txn) error {
        it := txn.NewIterator(badger.DefaultIteratorOptions)
        defer it.Close()
        prefix := []byte("cdisc:cache:")
        for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
            item := it.Item()
            val, err := item.ValueCopy(nil)
            if err != nil {
                continue
            }
            var entry cacheEntry
            if err := json.Unmarshal(val, &entry); err == nil && entry.Group == group {
                txn.Delete(item.KeyCopy(nil))
            }
        }
        return nil
    })
}

func (b *BadgerAdapter) CleanupExpired() (int64, error) {
    var count int64
    err := b.db.Update(func(txn *badger.Txn) error {
        it := txn.NewIterator(badger.DefaultIteratorOptions)
        defer it.Close()
        now := time.Now()
        for it.Rewind(); it.Valid(); it.Next() {
            item := it.Item()
            val, err := item.ValueCopy(nil)
            if err != nil {
                continue
            }
            var entry cacheEntry
            if err := json.Unmarshal(val, &entry); err == nil && now.After(entry.ExpiresAt) {
                txn.Delete(item.KeyCopy(nil))
                count++
            }
        }
        return nil
    })
    return count, err
}

func (b *BadgerAdapter) GetCache(key string) ([]byte, time.Time, error) {
    var entry cacheEntry
    err := b.db.View(func(txn *badger.Txn) error {
        item, err := txn.Get([]byte(key))
        if err != nil {
            return err
        }
        val, err := item.ValueCopy(nil)
        if err != nil {
            return err
        }
        return json.Unmarshal(val, &entry)
    })
    if err != nil {
        return nil, time.Time{}, err
    }
    return entry.Data, entry.ExpiresAt, nil
}


type PostgresAdapter struct {
	db *sql.DB
}

func (p *PostgresAdapter) UpsertCache(key string, data []byte, expiresAt time.Time, group string) error {
	_, err := p.db.Exec(`
		INSERT INTO cdisc_cache (key, data, expires_at, product_group) 
		VALUES ($1, $2, $3, $4) 
		ON CONFLICT (key) DO UPDATE SET data=$2, expires_at=$3, product_group=$4`,
		key, data, expiresAt, group)
	return err
}

func (p *PostgresAdapter) UpsertSystemMeta(key, val string) error {
	_, err := p.db.Exec(`
		INSERT INTO system_meta (key, val) VALUES ($1, $2) 
		ON CONFLICT (key) DO UPDATE SET val=$2`, key, val)
	return err
}

func (p *PostgresAdapter) UpsertProductMeta(id, ts string) error {
	_, err := p.db.Exec(`
		INSERT INTO product_meta (product_id, last_update) VALUES ($1, $2) 
		ON CONFLICT (product_id) DO UPDATE SET last_update=$2`, id, ts)
	return err
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

func (p *PostgresAdapter) GetCache(key string) ([]byte, time.Time, error) {
    var data []byte
    var expiresAt time.Time
    err := p.db.QueryRow("SELECT data, expires_at FROM cdisc_cache WHERE key=$1", key).
        Scan(&data, &expiresAt)
    return data, expiresAt, err
}


func parseTTL(ttl string) (time.Duration, error) {
    // Handle custom formats first
    if strings.HasSuffix(ttl, "y") {
        years, err := strconv.Atoi(strings.TrimSuffix(ttl, "y"))
        if err != nil {
            return 0, err
        }
        return time.Hour * 24 * 365 * time.Duration(years), nil
    }
    if strings.HasSuffix(ttl, "w") {
        weeks, err := strconv.Atoi(strings.TrimSuffix(ttl, "w"))
        if err != nil {
            return 0, err
        }
        return time.Hour * 24 * 7 * time.Duration(weeks), nil
    }
    if strings.HasSuffix(ttl, "d") {
        days, err := strconv.Atoi(strings.TrimSuffix(ttl, "d"))
        if err != nil {
            return 0, err
        }
        return time.Hour * 24 * time.Duration(days), nil
    }

    // Fall back to Go’s native duration parsing
    return time.ParseDuration(ttl)
}

// --- Logic: The Smart Cache Flow ---

func NewProxyServer(ctx context.Context, cfg Config) (*ProxyServer, error) {
	// Validate config
	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	// Parse TTLs
	l1TTL, err := parseTTL(cfg.Cache.L1.TTL)
	if err != nil {
		return nil, fmt.Errorf("invalid L1 TTL: %w", err)
	}
	l2TTL, err := parseTTL(cfg.Cache.L2.TTL)
	if err != nil {
		return nil, fmt.Errorf("invalid L2 TTL: %w", err)
	}

	// Create cancellable context
	serverCtx, cancel := context.WithCancel(ctx)

	ps := &ProxyServer{
		config:     cfg,
		httpClient: &http.Client{Timeout: requestTimeout},
		l1TTL:      l1TTL,
		l2TTL:      l2TTL,
		ctx:        serverCtx,
		cancel:     cancel,
	}

	// Init L1 (Valkey/Dragonfly)
	l1Client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress: []string{cfg.Cache.L1.Address},
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("L1 init failed: %w", err)
	}
	ps.l1Client = l1Client

	// Init L2 (BadgerDB or Postgres)
	var sqlDB *sql.DB
 	var storage StorageAdapter

	if cfg.Cache.L2.BadgerPath != "" {
		opts := badger.DefaultOptions(cfg.Cache.L2.BadgerPath).WithLogger(nil)
		badgerDB, err := badger.Open(opts)
		if err != nil {
			cancel()
			l1Client.Close()
			return nil, fmt.Errorf("Badger init failed: %w", err)
		}
		storage = &BadgerAdapter{db: badgerDB}
	} else {
		sqlDB, err = sql.Open("postgres", cfg.Cache.L2.PostgresDSN)
		if err != nil {
			cancel()
			l1Client.Close()
			return nil, fmt.Errorf("Postgres init failed: %w", err)
		}
		storage = &PostgresAdapter{db: sqlDB}

		// Test connection only for Postgres
		if err := sqlDB.Ping(); err != nil {
			cancel()
			l1Client.Close()
			sqlDB.Close()
			return nil, fmt.Errorf("L2 connection failed: %w", err)
		}
	}

	ps.l2Db = sqlDB   // will be nil if using Badger
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

func validateConfig(cfg Config) error {
	if cfg.Server.Port <= 0 || cfg.Server.Port > 65535 {
		return errors.New("invalid server port")
	}
	if cfg.Server.AuthKey == "" {
		return errors.New("auth_key is required")
	}
	if cfg.CDISC.BaseURL == "" {
		return errors.New("CDISC base_url is required")
	}
	if cfg.CDISC.APIKey == "" {
		return errors.New("CDISC api_key is required")
	}
	if cfg.Cache.L1.Address == "" {
		return errors.New("L1 address is required")
	}
	if cfg.Cache.L2.BadgerPath == "" && cfg.Cache.L2.PostgresDSN == "" {
		return errors.New("either BadgerDB path or Postgres DSN is required")
	}
	if cfg.Cache.L2.BadgerPath != "" && cfg.Cache.L2.PostgresDSN != "" {
		return errors.New("configure either BadgerDB or Postgres, not both")
	}
	return nil
}

func (ps *ProxyServer) saveToL1WithShortTTL(key string, data []byte, status int, ttl time.Duration) {
	cached := CachedResponse{
		StatusCode: status,
		Body:       data,
	}
	cachedBytes, err := json.Marshal(cached)
	if err != nil {
		log.Printf("Failed to marshal cached response: %v", err)
		return
	}

	ps.wg.Add(1)
	go func() {
		defer ps.wg.Done()
		ctx, cancel := context.WithTimeout(ps.ctx, 5*time.Second)
		defer cancel()
		if err := ps.l1Client.Do(ctx,
			ps.l1Client.B().Set().Key(key).Value(string(cachedBytes)).Ex(ttl).Build()).Error(); err != nil {
			log.Printf("L1 short-TTL save failed for %s: %v", key, err)
		}
	}()
}

func (ps *ProxyServer) Close() error {
	ps.cancel()
	ps.wg.Wait()

	var errs []error
	if ps.l1Client != nil {
		ps.l1Client.Close()
	}
	if ps.l2Db != nil {
		if err := ps.l2Db.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("close errors: %v", errs)
	}
	return nil
}

// Authentication middleware
func (ps *ProxyServer) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		expectedAuth := "Bearer " + ps.config.Server.AuthKey

		if authHeader != expectedAuth {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}

// handleProxy implements the Smart Logic
func (ps *ProxyServer) handleProxy(w http.ResponseWriter, r *http.Request) {
    path := strings.TrimPrefix(r.URL.Path, "/api")

    // Sanitize path
    if strings.Contains(path, "..") || !strings.HasPrefix(path, "/") {
        http.Error(w, "Invalid path", http.StatusBadRequest)
        return
    }

    cacheKey := "cdisc:cache:" + path
    prodGroup := ps.identifyProductGroup(path)

    // 1. L1 [hit] -> reply -> L1 [TTL update]
	// L1 HIT - Check for cached status
    val, err := ps.l1Client.Do(ps.ctx, ps.l1Client.B().Get().Key(cacheKey).Build()).AsBytes()
	if err == nil {
		var cached CachedResponse
		if err := json.Unmarshal(val, &cached); err == nil {
			// Successfully parsed cached response with status
			ps.wg.Add(1)
			go func() {
				defer ps.wg.Done()
				ctx, cancel := context.WithTimeout(ps.ctx, 5*time.Second)
				defer cancel()
				if err := ps.l1Client.Do(ctx,
					ps.l1Client.B().Expire().Key(cacheKey).Seconds(int64(ps.l1TTL.Seconds())).Build()).Error(); err != nil {
					log.Printf("L1 TTL update failed for %s: %v", cacheKey, err)
				}
			}()

			w.Header().Set("X-Cache-Tier", "L1-HIT")
			w.Header().Set("Content-Type", "application/json")
			if cached.StatusCode != 200 {
				w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			}
			w.WriteHeader(cached.StatusCode)
			w.Write(cached.Body)
			return
		}
		// Fall through if unmarshal fails (legacy cache format)
	}

    // 2. L1 [miss] -> L2 [hit] -> reply -> L1 [set]
	// L2 HIT - Also needs status code storage
	// For now, L2 only stores 200 responses, so this is safe
	data, expiresAt, err := ps.storage.GetCache(cacheKey)
	if err == nil && time.Now().Before(expiresAt) {
		// Promote back to L1 (as 200 response since L2 only stores successful responses)
		cached := CachedResponse{
			StatusCode: 200,
			Body:       data,
		}
		cachedBytes, _ := json.Marshal(cached)

		ps.wg.Add(1)
		go func() {
			defer ps.wg.Done()
			ctx, cancel := context.WithTimeout(ps.ctx, 5*time.Second)
			defer cancel()
			if err := ps.l1Client.Do(ctx,
				ps.l1Client.B().Set().Key(cacheKey).Value(string(cachedBytes)).Ex(ps.l1TTL).Build()).Error(); err != nil {
				log.Printf("L1 promotion failed for %s: %v", cacheKey, err)
			}
		}()

		w.Header().Set("X-Cache-Tier", "L2-HIT")
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
		return
	}

	respBody, status, err := ps.fetchFromCDISC(path)
	if err != nil {
		log.Printf("CDISC API error for %s: %v", path, err)
		http.Error(w, "CDISC API Error", http.StatusBadGateway)
		return
	}

	// 3. L1 [miss] -> L2 [miss] -> upstream -> reply -> L1 [set] -> L2 [set]
	// Save with status code

	
	if status == 200 {
		// Save to L2 (Persistence) - only 200s
		ps.mu.Lock()
		if err := ps.saveToL2(cacheKey, respBody, prodGroup); err != nil {
			log.Printf("L2 save failed for %s: %v", cacheKey, err)
		}
		ps.mu.Unlock()

		// Save to L1 (RAM) with status code
		cached := CachedResponse{
			StatusCode: status,
			Body:       respBody,
		}
		cachedBytes, err := json.Marshal(cached)
		if err == nil {
			ps.wg.Add(1)
			go func() {
				defer ps.wg.Done()
				ctx, cancel := context.WithTimeout(ps.ctx, 5*time.Second)
				defer cancel()
				if err := ps.l1Client.Do(ctx,
					ps.l1Client.B().Set().Key(cacheKey).Value(string(cachedBytes)).Ex(ps.l1TTL).Build()).Error(); err != nil {
					log.Printf("L1 save failed for %s: %v", cacheKey, err)
				}
			}()
		}

		w.Header().Set("X-Cache-Tier", "MISS")
	} else if status == 400 || status == 404 {
		// Save to L1 with short TTL
		ps.saveToL1WithShortTTL(cacheKey, respBody, status, 30*time.Minute)
		w.Header().Set("X-Cache-Tier", "MISS")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	} else {
		// Don't cache other errors (5xx, etc.)
		w.Header().Set("X-Cache-Tier", "MISS")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(respBody)
}

func (ps *ProxyServer) fetchFromCDISC(path string) ([]byte, int, error) {
	url := ps.config.CDISC.BaseURL + path
	req, err := http.NewRequestWithContext(ps.ctx, "GET", url, nil)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Api-Key", ps.config.CDISC.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := ps.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	// Limit response size
	limitedReader := io.LimitReader(resp.Body, maxResponseSize)
	body, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, 0, err
	}

	return body, resp.StatusCode, nil
}

// --- Logic: Smart Invalidation Scheduler ---

func (ps *ProxyServer) runScheduler() {
	defer ps.wg.Done()

	interval, err := time.ParseDuration(ps.config.Scheduler.Interval)
	if err != nil {
		log.Printf("Invalid scheduler interval: %v", err)
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ps.ctx.Done():
			log.Println("[Scheduler] Shutting down")
			return
		case <-ticker.C:
			log.Println("[Scheduler] Checking CDISC Library for updates...")
			if err := ps.checkAndSync(); err != nil {
				log.Printf("[Scheduler] Sync error: %v", err)
			}
		}
	}
}

func (ps *ProxyServer) checkAndSync() error {
	// 1. Check Global /mdr/lastupdated
	body, status, err := ps.fetchFromCDISC("/mdr/lastupdated")
	if err != nil || status != 200 {
		return fmt.Errorf("failed to fetch last updated: %w", err)
	}

	var global struct {
		LastUpdated string `json:"lastUpdated"`
	}
	if err := json.Unmarshal(body, &global); err != nil {
		return fmt.Errorf("failed to parse last updated: %w", err)
	}

	var lastSeen string
	err = ps.l2Db.QueryRowContext(ps.ctx, "SELECT val FROM system_meta WHERE key = 'global_ts'").Scan(&lastSeen)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("failed to query global_ts: %w", err)
	}

	if global.LastUpdated != lastSeen {
		log.Printf("[Scheduler] Update detected: %s. Syncing products...", global.LastUpdated)
		if err := ps.syncAndInvalidateProducts(global.LastUpdated); err != nil {
			return err
		}
	}

	return nil
}

func (ps *ProxyServer) syncAndInvalidateProducts(newGlobalTs string) error {
	body, status, err := ps.fetchFromCDISC("/mdr/products")
	if err != nil || status != 200 {
		return fmt.Errorf("failed to fetch products: %w", err)
	}

	var catalog struct {
		Products []struct {
			ID          string `json:"id"`
			LastUpdated string `json:"lastUpdated"`
		} `json:"products"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		return fmt.Errorf("failed to parse products: %w", err)
	}

	for _, p := range catalog.Products {
		var localTs string
		err := ps.l2Db.QueryRowContext(ps.ctx, "SELECT last_update FROM product_meta WHERE product_id = $1", p.ID).Scan(&localTs)
		if err != nil && err != sql.ErrNoRows {
			log.Printf("[Scheduler] Error checking product %s: %v", p.ID, err)
			continue
		}

		if p.LastUpdated != localTs {
			log.Printf("[Scheduler] Invalidating out-of-date product: %s", p.ID)

			// SQL Invalidation
			ps.mu.Lock()
			if err := ps.storage.DeleteByProductGroup(strings.ToLower(p.ID)); err != nil {
				log.Printf("[Scheduler] L2 invalidation failed for %s: %v", p.ID, err)
			}
			ps.mu.Unlock()

			// Smart L1 Invalidation
			if err := ps.invalidateL1ByPrefix("cdisc:cache:/mdr/" + strings.ToLower(p.ID)); err != nil {
				log.Printf("[Scheduler] L1 invalidation failed for %s: %v", p.ID, err)
			}

			// Update Metadata
			if err := ps.storage.UpsertProductMeta(p.ID, p.LastUpdated); err != nil {
				log.Printf("[Scheduler] Product meta update failed for %s: %v", p.ID, err)
			}
		}
	}

	if err := ps.storage.UpsertSystemMeta("global_ts", newGlobalTs); err != nil {
		return fmt.Errorf("failed to update global_ts: %w", err)
	}

	return nil
}

func (ps *ProxyServer) invalidateL1ByPrefix(prefix string) error {
	// For Dragonfly, we use larger batch sizes to leverage its multi-threaded DEL
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

		res, err := ps.l1Client.Do(
			ps.ctx,
			ps.l1Client.B().
				Scan().
				Cursor(cursor).
				Match(prefix + "*").
				Count(int64(batchSize)).
				Build(),
		).AsScanEntry()
		if err != nil {
			return fmt.Errorf("scan failed: %w", err)
		}

		if len(res.Elements) == 0 {
			break
		}

		var delErr error
		if ps.config.Cache.L1.Driver == "dragonfly" {
			delErr = ps.l1Client.Do(
				ps.ctx,
				ps.l1Client.B().Del().Key(res.Elements...).Build(),
			).Error()
		} else {
			delErr = ps.l1Client.Do(
				ps.ctx,
				ps.l1Client.B().Unlink().Key(res.Elements...).Build(),
			).Error()
		}

		if delErr != nil {
			return fmt.Errorf("delete failed: %w", delErr)
		}

		cursor = res.Cursor
		if cursor == 0 {
			break
		}
	}

	return nil
}

// --- L2 Cleanup Job ---

func (ps *ProxyServer) runCleanup() {
	defer ps.wg.Done()

	interval, err := time.ParseDuration(ps.config.Cache.L2.CleanupInterval)
	if err != nil {
		log.Printf("Invalid cleanup interval, using default 1h: %v", err)
		interval = time.Hour
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ps.ctx.Done():
			log.Println("[Cleanup] Shutting down")
			return
		case <-ticker.C:
			log.Println("[Cleanup] Running expired entry cleanup...")
			ps.mu.Lock()
			count, err := ps.storage.CleanupExpired()
			ps.mu.Unlock()

			if err != nil {
				log.Printf("[Cleanup] Error: %v", err)
			} else {
				log.Printf("[Cleanup] Removed %d expired entries", count)
			}
		}
	}
}

// --- Helpers: DB, Config, and Extraction ---

func (ps *ProxyServer) identifyProductGroup(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/mdr/"), "/")
	if len(parts) > 0 {
		return strings.ToLower(parts[0])
	}
	return "misc"
}

func (ps *ProxyServer) saveToL2(key string, data []byte, group string) error {
	expiry := time.Now().Add(ps.l2TTL)
	return ps.storage.UpsertCache(key, data, expiry, group)
}

func main() {
	cfgFile := "/etc/conf.d/cdisc-proxy.conf"

	if len(os.Args) > 1 {
		cfgFile = os.Args[1]
	}


	data, err := os.ReadFile(cfgFile)
	if err != nil {
		log.Fatalf("Failed to read config: %v", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("Failed to parse config: %v", err)
	}

	ctx := context.Background()
	server, err := NewProxyServer(ctx, cfg)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}
	defer server.Close()

	// Setup routes with auth middleware
	// http.HandleFunc("/api/", server.authMiddleware(server.handleProxy))

	// Setup routes without auth middleware
	http.HandleFunc("/api/", server.handleProxy)

	// Health check endpoint (no auth required)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"healthy"}`))
	})


	
	// --- Multi-interface binding ---
	listenAddrs := cfg.Server.Listen
	if len(listenAddrs) == 0 {
		 listenAddrs = []string{"0.0.0.0"} // default 
	}
	
	for _, addr := range listenAddrs {
		go func(addr string) {
			bind := fmt.Sprintf("%s:%d", addr, cfg.Server.Port)
			log.Printf("Starting server on %s", bind)
			if err := http.ListenAndServe(bind, nil); err != nil {
				log.Fatalf("Server failed on %s: %v", bind, err)
			}
		}(addr)
	}
	
	// Block forever select {}
	select {}
}
