use anyhow::{anyhow, Result};
use async_trait::async_trait;
use axum::{
    body::Body,
    extract::{Request, State},
    http::StatusCode,
    response::{IntoResponse, Response},
};
use bincode;
use fred::prelude::{Client as RedisClient, Config as RedisConfig, ClientLike, KeysInterface, Expiration};
use sled::Db as SledDb;
use serde::{Deserialize, Serialize};
use sqlx::{postgres::PgPoolOptions, PgPool};
use std::{
    collections::HashMap,
    hash::{Hash, Hasher},
    path::PathBuf,
    sync::Arc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use tokio::{
    fs,
};

use dashmap::DashMap;

use tokio::sync::{RwLock, Semaphore, watch};

pub mod cache_key;
use cache_key::canonical_cache_key;

// Constants
pub const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
pub const MAX_CONCURRENT_REQS: usize = 10000;
pub const NEGATIVE_CACHE_TTL_4XX: Duration = Duration::from_secs(600);
pub const NEGATIVE_CACHE_TTL_OTHER: Duration = Duration::from_secs(300);
pub const MAX_PATH_LENGTH: usize = 512;
pub const MAX_L1_OBJECT_SIZE: usize = 2 * 1024 * 1024; // 2 MB
pub const MAX_L2_OBJECT_SIZE: usize = 10 << 20; // 10 MB


// Configuration
#[allow(dead_code)]
#[derive(Debug, Clone, Deserialize)]
pub struct AppConfig {
    pub server: ServerConfig,
    pub cdisc: CdiscConfig,
    pub cache: CacheConfig,
    pub scheduler: SchedulerConfig,
}

#[allow(dead_code)]
#[derive(Debug, Clone, Deserialize)]
pub struct ServerConfig {
    pub port: u16,
    pub listen: Vec<String>,
    pub auth_key: Option<String>,
}

#[allow(dead_code)]
#[derive(Debug, Clone, Deserialize)]
pub struct CdiscConfig {
    base_url: String,
    api_key: String,
}

#[allow(dead_code)]
#[derive(Debug, Clone, Deserialize)]
pub struct CacheConfig {
    l1: L1Config,
    l2: L2Config,
}

#[allow(dead_code)]
#[derive(Debug, Clone, Deserialize)]
pub struct L1Config {
    driver: String,
    address: String,
    ttl: String,
}

#[allow(dead_code)]
#[derive(Debug, Clone, Deserialize)]
pub struct L2Config {
    storage_path: Option<String>,
    postgres_dsn: Option<String>,
    ttl: String,
    cleanup_enabled: bool,
    cleanup_interval: String,
}

#[allow(dead_code)]
#[derive(Debug, Clone, Deserialize)]
pub struct SchedulerConfig {
    enabled: bool,
    interval: String,
}

// Cached Response
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CachedResponse {
    status_code: u16,
    headers: HashMap<String, String>,
    body: Vec<u8>,
    expires_at: u64, // unix seconds
}

// Health Status
#[derive(Debug, Serialize)]
struct HealthStatus {
    status: String,
    timestamp: SystemTime,
    #[allow(dead_code)]
    details: HashMap<String, String>,
}

// Storage Adapter Trait
#[async_trait]
pub trait StorageAdapter: Send + Sync {
    async fn upsert_cache(
        &self,
        key: &str,
        data: &[u8],
        expires_at: SystemTime,
        group: &str,
    ) -> Result<()>;
    async fn get_cache(&self, key: &str) -> Result<(Vec<u8>, SystemTime)>;
    async fn delete_by_product_group(&self, group: &str) -> Result<()>;
    async fn cleanup_expired(&self) -> Result<i64>;
    async fn get_system_meta(&self, key: &str) -> Result<Option<String>>;
    async fn upsert_system_meta(&self, key: &str, val: &str) -> Result<()>;
    async fn ping(&self) -> Result<()>;
}

// Evictions
#[allow(dead_code)]
#[derive(Debug, Clone)]
struct DiskEviction {
    max_bytes: u64,
    target_bytes: u64,
    scan_limit: usize,
}


// Sled + Filesystem Blob Adapter
struct SledBlobAdapter {
    db: Arc<SledDb>,
    blob_dir: PathBuf,
    eviction: DiskEviction,
}

#[derive(Debug, Serialize, Deserialize)]
struct CacheMetadata {
    expires_at: u64, // Unix timestamp
    size: usize,
    is_blob: bool, // true if data is in filesystem, false if inline
}

impl CdiscConfig {
    pub fn new(base_url: impl Into<String>, api_key: impl Into<String>) -> Self {
        Self {
            base_url: base_url.into(),
            api_key: api_key.into(),
        }
    }
}

impl ServerConfig {
    pub fn new(port: u16, listen: Vec<String>, auth_key: Option<String>) -> Self {
        Self { port, listen, auth_key }
    }
}

impl CacheConfig {
    pub fn new(l1: L1Config, l2: L2Config) -> Self {
        Self { l1, l2 }
    }
}

impl L1Config {
    pub fn new(driver: impl Into<String>, address: impl Into<String>, ttl: impl Into<String>) -> Self {
        Self {
            driver: driver.into(),
            address: address.into(),
            ttl: ttl.into(),
        }
    }
}

impl L2Config {
    pub fn new(
        storage_path: Option<String>,
        postgres_dsn: Option<String>,
        ttl: impl Into<String>,
        cleanup_enabled: bool,
        cleanup_interval: impl Into<String>,
    ) -> Self {
        Self {
            storage_path,
            postgres_dsn,
            ttl: ttl.into(),
            cleanup_enabled,
            cleanup_interval: cleanup_interval.into(),
        }
    }
}

impl SchedulerConfig {
    pub fn new(enabled: bool, interval: impl Into<String>) -> Self {
        Self {
            enabled,
            interval: interval.into(),
        }
    }
}

impl AppConfig {
    pub fn new(
        server: ServerConfig,
        cdisc: CdiscConfig,
        cache: CacheConfig,
        scheduler: SchedulerConfig,
    ) -> Self {
        Self {
            server,
            cdisc,
            cache,
            scheduler,
        }
    }
}


impl SledBlobAdapter {
    async fn new(path: impl Into<PathBuf>) -> Result<Self> {
        let base_path: PathBuf = path.into();
        let db_path = base_path.join("sled");
        let blob_dir = base_path.join("blobs");

        fs::create_dir_all(&db_path).await?;
        fs::create_dir_all(&blob_dir).await?;

        let db = sled::open(&db_path)?;

        Ok(Self {
            db: Arc::new(db),
            blob_dir,
            eviction: DiskEviction {
                max_bytes: 500 * 1024 * 1024 * 1024,   // 500GB
                target_bytes: 450 * 1024 * 1024 * 1024,
                scan_limit: 10_000,
            },
        })
    }
 
    /// Produce a canonical byte representation for cache keys.
    /// All code that writes/reads sled keys or computes blob filenames
    /// must use this function so hashing and lookups are deterministic.
    fn canonical_key_bytes(key: &str) -> Vec<u8> {
        // Normalize path: trim, collapse repeated slashes, and use UTF-8 bytes.
        // If you need additional normalization (percent-decoding, case folding),
        // add it here so it applies everywhere.
        //
        // Keep this function minimal and deterministic.
        let mut s = key.trim().to_string();
        // Collapse duplicate slashes (e.g., "//" -> "/")
        while s.contains("//") {
            s = s.replace("//", "/");
        }
        s.into_bytes()
    }

    fn blob_disk_usage(dir: &PathBuf) -> u64 {
        let mut total = 0;
        if let Ok(entries) = std::fs::read_dir(dir) {
            for e in entries.flatten() {
                let p = e.path();
                if p.is_dir() {
                    total += Self::blob_disk_usage(&p);
                } else if let Ok(m) = std::fs::metadata(&p) {
                    total += m.len();
                }
            }
        }
        total
    }

    fn evict_under_pressure(&self) -> Result<u64> {
        let mut total = Self::blob_disk_usage(&self.blob_dir);
        if total <= self.eviction.max_bytes {
            return Ok(0);
        }

        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)?
            .as_secs();

        let mut candidates = Vec::new();

        // bounded scan
        for result in self.db.scan_prefix(b"cdisc:cache:") {
            let (key, value) = result?;

            if value.len() < 4 {
                continue;
            }

            let meta_len = u32::from_le_bytes(value[..4].try_into()?) as usize;
            if value.len() < 4 + meta_len {
                continue;
            }

            let meta: CacheMetadata =
                match bincode::deserialize(&value[4..4 + meta_len]) {
                    Ok(m) => m,
                    Err(_) => continue,
                };

            if !meta.is_blob {
                continue;
            }

            candidates.push((key.to_vec(), meta));

            if candidates.len() >= self.eviction.scan_limit {
                break;
            }
        }

        // expired first
        candidates.sort_by(|a, b| {
            let ae = a.1.expires_at <= now;
            let be = b.1.expires_at <= now;
            be.cmp(&ae).then(a.1.expires_at.cmp(&b.1.expires_at))
        });

        let mut freed = 0;

        for (key, meta) in candidates {
            if total <= self.eviction.target_bytes {
                break;
            }

            // remove sled entry first
            let _ = self.db.remove(&key);

            // compute blob path
            let blob_path = self.get_blob_path_for_key_bytes(&key);
            let mut freed_bytes = meta.size as u64;

            if let Ok(md) = std::fs::metadata(&blob_path) {
                freed_bytes = md.len();
                freed += freed_bytes;
            }
            let _ = std::fs::remove_file(blob_path);

            total = total.saturating_sub(freed_bytes);
        }

        self.db.flush()?;
        Ok(freed)
    }

    fn normalize_key_bytes(key: &str) -> Vec<u8> {
        // Delegate to canonical_key_bytes so all callers share the same canonicalization.
        Self::canonical_key_bytes(key)
    }

    fn get_blob_path_for_key_bytes(&self, key: &[u8]) -> PathBuf {
        // Hash the key to create a filename (deterministic)
        let mut hasher = std::collections::hash_map::DefaultHasher::new();
        key.hash(&mut hasher);
        let hash = hasher.finish();

        // Use one-byte subdir to avoid too many files in a single dir; adjust if needed
        let subdir = format!("{:02x}", (hash >> 56) & 0xFF);
        let filename = format!("{:016x}.blob", hash);

        self.blob_dir.join(subdir).join(filename)
    }

    /// Compute blob path from blob_dir and raw key bytes (same hashing as above).
    /// This is a pure helper so blocking closures that don't have `self` can still
    /// compute the exact same filename.
    fn compute_blob_path(blob_dir: &PathBuf, key_bytes: &[u8]) -> PathBuf {
        let mut hasher = std::collections::hash_map::DefaultHasher::new();
        key_bytes.hash(&mut hasher);
        let hash = hasher.finish();
        let subdir = format!("{:02x}", (hash >> 56) & 0xFF);
        let filename = format!("{:016x}.blob", hash);
        blob_dir.join(subdir).join(filename)
    }

    const BLOB_THRESHOLD: usize = 1024 * 1024; // 1 MB

    /// Canonical on-disk value format in sled:
    /// [4-byte little-endian meta_len][meta_bytes (bincode)][optional inline data bytes]
    fn build_sled_value(meta: &CacheMetadata, inline_data: Option<&[u8]>) -> Result<Vec<u8>> {
        let meta_bytes = bincode::serialize(meta)?;
        let mut value = Vec::with_capacity(4 + meta_bytes.len() + inline_data.map(|d| d.len()).unwrap_or(0));
        let meta_len = (meta_bytes.len() as u32).to_le_bytes();
        value.extend_from_slice(&meta_len);
        value.extend_from_slice(&meta_bytes);
        if let Some(d) = inline_data {
            value.extend_from_slice(d);
        }
        Ok(value)
    }
}

#[async_trait]
impl StorageAdapter for SledBlobAdapter {
    async fn upsert_cache(
        &self,
        key: &str,
        data: &[u8],
        expires_at: SystemTime,
        _group: &str,
    ) -> Result<()> {
        let key_bytes = Self::normalize_key_bytes(key);
        let ttl = expires_at
            .duration_since(SystemTime::now())
            .unwrap_or_default();

        if ttl.is_zero() {
            return Err(anyhow!("expiration in past"));
        }

        let expires_ts = expires_at.duration_since(UNIX_EPOCH)?.as_secs();
        let is_blob = data.len() > Self::BLOB_THRESHOLD;

        let metadata = CacheMetadata {
            expires_at: expires_ts,
            size: data.len(),
            is_blob,
        };

        let db = self.db.clone();
        let db_eviction = db.clone();
        let key_clone = key_bytes.clone();
        let blob_dir = self.blob_dir.clone();
        let eviction = self.eviction.clone();



        if is_blob {
            // Store data in filesystem (async)
            let blob_path = self.get_blob_path_for_key_bytes(&key_clone);
            // Ensure parent exists
            if let Some(parent) = blob_path.parent() {
                tokio::fs::create_dir_all(parent).await?;
            }
            let tmp = blob_path.with_extension("tmp");
            tokio::fs::write(&tmp, data).await?;
            tokio::fs::rename(&tmp, &blob_path).await?;

            // Store metadata in sled (no inline data)
            let value = Self::build_sled_value(&metadata, None)?;
            tokio::task::spawn_blocking(move || {
                db.insert(key_clone, value)?;
                db.flush()?;
                Ok::<(), anyhow::Error>(())
            })
            .await??;
        } else {
            // Store inline with metadata prefix
            let value = Self::build_sled_value(&metadata, Some(data))?;
            tokio::task::spawn_blocking(move || {
                db.insert(key_clone, value)?;
                db.flush()?;
                Ok::<(), anyhow::Error>(())
            })
            .await??;
        }

        // Trigger eviction in background without awaiting
        // Trigger eviction in background without awaiting (runs on blocking pool)
        tokio::task::spawn_blocking(move || {
            let adapter = SledBlobAdapter { db: db_eviction, blob_dir, eviction };
            let _ = adapter.evict_under_pressure();
        });

        Ok(())
    }

    async fn get_cache(&self, key: &str) -> Result<(Vec<u8>, SystemTime)> {
        let key_bytes = Self::normalize_key_bytes(key);
        let key_bytes_for_blocking = key_bytes.clone();
        let db = self.db.clone();

        let opt_value = tokio::task::spawn_blocking(move || db.get(&key_bytes_for_blocking)).await??;
        let value = opt_value.ok_or_else(|| anyhow!("key not found"))?;

        // Read metadata length prefix (4 bytes)
        if value.len() < 4 {
            return Err(anyhow!("invalid cache entry"));
        }

        let meta_len_bytes: [u8; 4] = value[..4].try_into()?;
        let meta_len = u32::from_le_bytes(meta_len_bytes) as usize;

        if value.len() < 4 + meta_len {
            return Err(anyhow!("invalid cache entry: truncated metadata"));
        }

        let metadata: CacheMetadata = bincode::deserialize(&value[4..4 + meta_len])?;

        // Check expiration
        let expires_at = UNIX_EPOCH + Duration::from_secs(metadata.expires_at);
        if expires_at < SystemTime::now() {
            // Cleanup expired entry synchronously
            let key_clone = key_bytes.clone();
            let key_clone_for_hash = key_bytes.clone();
            let db_clone = self.db.clone();
            let blob_dir = self.blob_dir.clone();
            
            // We use spawn_blocking to avoid blocking the async executor with sled/fs ops
            // but we await it to ensure it completes before returning.
            let _ = tokio::task::spawn_blocking(move || {
                if db_clone.remove(&key_clone).is_ok() {
                    let _ = db_clone.flush();
                }
                // Compute blob path inside blocking task or using helper
                let blob_path = SledBlobAdapter::compute_blob_path(&blob_dir, &key_clone_for_hash);
                let _ = std::fs::remove_file(blob_path);
                Ok::<(), anyhow::Error>(())
            })
            .await;

            return Err(anyhow!("key expired"));
        }

        let data = if metadata.is_blob {
            // Read from filesystem
            let blob_path = self.get_blob_path_for_key_bytes(&key_bytes);
            fs::read(&blob_path).await?
        } else {
            // Data is inline
            value[4 + meta_len..].to_vec()
        };

        Ok((data, expires_at))
    }

    async fn delete_by_product_group(&self, group: &str) -> Result<()> {
        // Expect keys to be prefixed like "cdisc:cache:{group}:..."
        let prefix_str = format!("cdisc:cache:{}:", group.to_lowercase());
        let prefix = SledBlobAdapter::canonical_key_bytes(&prefix_str);
        let db = self.db.clone();
        let blob_dir = self.blob_dir.clone();

        tokio::task::spawn_blocking(move || {
            let mut keys_to_delete = Vec::new();

            // Scan for matching keys
            for result in db.scan_prefix(&prefix) {
                let (key, value) = result?;
                keys_to_delete.push((key.to_vec(), value.to_vec()));
            }

            // Delete keys and associated blobs
            for (key, value) in keys_to_delete {
                // Parse metadata safely
                if value.len() >= 4 {
                    if let Ok(meta_len_bytes) = value[..4].try_into() {
                        let meta_len = u32::from_le_bytes(meta_len_bytes) as usize;
                        if value.len() >= 4 + meta_len {
                            if let Ok(metadata) = bincode::deserialize::<CacheMetadata>(&value[4..4 + meta_len]) {
                                if metadata.is_blob {
                                    // Delete blob file
                                    let blob_path = SledBlobAdapter::compute_blob_path(&blob_dir, &key);
                                    let _ = std::fs::remove_file(blob_path);
                                }
                            }
                        }
                    }
                }

                db.remove(&key)?;
            }

            db.flush()?;
            Ok::<_, anyhow::Error>(())
        })
        .await??;

        Ok(())
    }

    async fn cleanup_expired(&self) -> Result<i64> {
        let db = self.db.clone();
        let blob_dir = self.blob_dir.clone();
        let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs();

        let count = tokio::task::spawn_blocking(move || {
            let mut deleted = 0i64;
            let mut keys_to_delete = Vec::new();

            // Scan all keys with canonical "cdisc:cache:" prefix
            let prefix = SledBlobAdapter::canonical_key_bytes("cdisc:cache:");
            for result in db.scan_prefix(&prefix) {
                let (key, value) = match result {
                    Ok(kv) => kv,
                    Err(_) => continue,
                };

                // Parse metadata
                if value.len() < 4 {
                    continue;
                }

                let meta_len_bytes: [u8; 4] = match value[..4].try_into() {
                    Ok(b) => b,
                    Err(_) => continue,
                };
                let meta_len = u32::from_le_bytes(meta_len_bytes) as usize;

                if value.len() < 4 + meta_len {
                    continue;
                }

                let metadata: CacheMetadata = match bincode::deserialize(&value[4..4 + meta_len]) {
                    Ok(m) => m,
                    Err(_) => continue,
                };

                // Check if expired
                if metadata.expires_at < now {
                    keys_to_delete.push((key.to_vec(), metadata.is_blob));
                }
            }

            // Delete expired entries
            for (key, is_blob) in keys_to_delete {
                if is_blob {
                    // Delete blob file
                    let blob_path = SledBlobAdapter::compute_blob_path(&blob_dir, &key);
                    let _ = std::fs::remove_file(blob_path);
                }

                if db.remove(&key).is_ok() {
                    deleted += 1;
                }
            }

            let _ = db.flush();
            Ok::<i64, anyhow::Error>(deleted)
        })
        .await??;

        Ok(count)
    }

    async fn get_system_meta(&self, key: &str) -> Result<Option<String>> {
        let full_key = format!("system:{}", key).into_bytes();
        let db = self.db.clone();

        let result = tokio::task::spawn_blocking(move || db.get(&full_key)).await??;

        match result {
            Some(v) => Ok(Some(String::from_utf8(v.to_vec())?)),
            None => Ok(None),
        }
    }

    async fn upsert_system_meta(&self, key: &str, val: &str) -> Result<()> {
        let full_key = format!("system:{}", key).into_bytes();
        let val_bytes = val.as_bytes().to_vec();
        let db = self.db.clone();

        tokio::task::spawn_blocking(move || {
            db.insert(&full_key, val_bytes)?;
            db.flush()?;
            Ok::<_, anyhow::Error>(())
        })
        .await??;

        Ok(())
    }

    async fn ping(&self) -> Result<()> {
        let db = self.db.clone();
        tokio::task::spawn_blocking(move || {
            // Simple test operation
            let _ = db.get(b"__ping_test__")?;
            Ok::<_, anyhow::Error>(())
        })
        .await??;
        Ok(())
    }
}

// PostgreSQL Adapter
struct PostgresAdapter {
    pool: PgPool,
}

impl PostgresAdapter {
    async fn new(dsn: &str) -> Result<Self> {
        let pool = PgPoolOptions::new()
            .max_connections(25)
            .idle_timeout(Duration::from_secs(120))
            .connect(dsn)
            .await?;

        Ok(Self { pool })
    }
}

#[async_trait]
impl StorageAdapter for PostgresAdapter {
    async fn upsert_cache(
        &self,
        key: &str,
        data: &[u8],
        expires_at: SystemTime,
        group: &str,
    ) -> Result<()> {
        let expires = expires_at.duration_since(UNIX_EPOCH)?.as_secs() as i64;

        sqlx::query(
            r#"
            INSERT INTO cdisc_cache (key, data, expires_at, product_group)
            VALUES ($1, $2, to_timestamp($3), $4)
            ON CONFLICT (key) DO UPDATE
            SET data = $2, expires_at = to_timestamp($3), product_group = $4
            "#,
        )
        .bind(key)
        .bind(data)
        .bind(expires)
        .bind(group)
        .execute(&self.pool)
        .await?;

        Ok(())
    }

    async fn get_cache(&self, key: &str) -> Result<(Vec<u8>, SystemTime)> {
        let row: (Vec<u8>, Option<i64>) = sqlx::query_as(
            r#"
            SELECT data, EXTRACT(EPOCH FROM expires_at)::bigint as expires_at
            FROM cdisc_cache
            WHERE key = $1 AND expires_at > NOW()
            "#,
        )
        .bind(key)
        .fetch_one(&self.pool)
        .await?;

        let expires_at = UNIX_EPOCH + Duration::from_secs(row.1.unwrap_or(0) as u64);

        Ok((row.0, expires_at))
    }

    async fn delete_by_product_group(&self, group: &str) -> Result<()> {
        sqlx::query("DELETE FROM cdisc_cache WHERE product_group = $1")
            .bind(group)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    async fn cleanup_expired(&self) -> Result<i64> {
        let result = sqlx::query("DELETE FROM cdisc_cache WHERE expires_at < NOW()")
            .execute(&self.pool)
            .await?;
        Ok(result.rows_affected() as i64)
    }

    async fn get_system_meta(&self, key: &str) -> Result<Option<String>> {
        let row: Option<(String,)> = sqlx::query_as("SELECT val FROM system_meta WHERE key = $1")
            .bind(key)
            .fetch_optional(&self.pool)
            .await?;
        Ok(row.map(|r| r.0))
    }

    async fn upsert_system_meta(&self, key: &str, val: &str) -> Result<()> {
        sqlx::query(
            r#"
            INSERT INTO system_meta (key, val) VALUES ($1, $2)
            ON CONFLICT (key) DO UPDATE SET val = $2
            "#,
        )
        .bind(key)
        .bind(val)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    async fn ping(&self) -> Result<()> {
        sqlx::query("SELECT 1").execute(&self.pool).await?;
        Ok(())
    }
}

// Health Metrics
#[derive(Default)]
struct HealthMetrics {
    #[allow(dead_code)]
    cdisc_failures: usize,
    #[allow(dead_code)]
    last_cdisc_fail: Option<SystemTime>,
}

// Proxy Server State
pub struct ProxyServer {
    config: AppConfig,
    l1_client: RedisClient,
    storage: Arc<dyn StorageAdapter>,
    http_client: reqwest::Client,
    l1_ttl: Duration,
    l2_ttl: Duration,
    #[allow(dead_code)]
    l2_sem: Arc<Semaphore>,
    #[allow(dead_code)]
    health: Arc<RwLock<HealthMetrics>>,
    inflight: DashMap<String, watch::Receiver<Option<CachedResponse>>>,
    upstream_sem: Arc<Semaphore>, //control parallel upstream requests
}

impl ProxyServer {
    pub async fn new(config: AppConfig) -> Result<Arc<Self>> {
        validate_config(&config)?;

        let l1_ttl = parse_ttl(&config.cache.l1.ttl)?;
        let l2_ttl = parse_ttl(&config.cache.l2.ttl)?;

        // Initialize L1 (Redis/Valkey)
        let redis_config = RedisConfig::from_url(&config.cache.l1.address)?;
        let l1_client = RedisClient::new(redis_config, None, None, None);
        l1_client.connect();
        l1_client.wait_for_connect().await?;

        // Initialize L2
        let storage: Arc<dyn StorageAdapter> = if let Some(path) = &config.cache.l2.storage_path {
            Arc::new(SledBlobAdapter::new(path).await?)
        } else if let Some(dsn) = &config.cache.l2.postgres_dsn {
            Arc::new(PostgresAdapter::new(dsn).await?)
        } else {
            return Err(anyhow!("No L2 storage configured"));
        };

        let http_client = reqwest::Client::builder()
            .timeout(REQUEST_TIMEOUT)
            .pool_max_idle_per_host(20)
            .pool_idle_timeout(Duration::from_secs(90))
            .build()?;

        Ok(Arc::new(Self {
            config,
            l1_client,
            storage,
            http_client,
            l1_ttl,
            l2_ttl,
            l2_sem: Arc::new(Semaphore::new(MAX_CONCURRENT_REQS)),
            health: Arc::new(RwLock::new(HealthMetrics::default())),
            inflight: DashMap::new(),
            upstream_sem: Arc::new(Semaphore::new(10)), //max 10 parallel requests to upstream
        }))
    }

    pub async fn handle_proxy(
        State(server): State<Arc<ProxyServer>>,
        req: Request,
    ) -> Result<Response, StatusCode> {

        if req.method() != axum::http::Method::GET {
            return Err(StatusCode::METHOD_NOT_ALLOWED);
        }

        let path_and_query = req.uri().path_and_query().map(|p| p.as_str()).unwrap_or("");
        let path_and_query = if path_and_query.starts_with("/api") {
            path_and_query.strip_prefix("/api").unwrap_or(path_and_query)
        } else {
            path_and_query
        };
        let path = req.uri().path();
        let path = if path.starts_with("/api") {
            path.strip_prefix("/api").unwrap_or(path)
        } else {
            path
        };

        let (ns, valid) = identify_namespace(path);
        if !valid {
            return Err(StatusCode::BAD_REQUEST);
        }

        let cache_key = canonical_cache_key(path_and_query, &ns);

        // L1
        if let Ok(cached) = server.get_from_l1(&cache_key).await {
            return Ok(server.write_response(cached, "L1-HIT"));
        }

        // L2
        if let Ok((data, _)) = server.storage.get_cache(&cache_key).await {
            if let Ok(cached) = serde_json::from_slice::<CachedResponse>(&data) {
                server.async_save_to_l1(&cache_key, &cached).await;
                return Ok(server.write_response(cached, "L2-HIT"));
            }
        }

        // Single-flight
        let (mut rx, is_leader, tx_opt) = match server.inflight.entry(cache_key.clone()) {
            dashmap::mapref::entry::Entry::Occupied(e) => {
                // Follower
                (e.get().clone(), false, None)
            }
            dashmap::mapref::entry::Entry::Vacant(e) => {
                // Leader
                // Create a channel
                let (tx, rx) = watch::channel(None);
                e.insert(rx.clone());
                (rx, true, Some(tx))
            }
        };

        // Follower path
        if !is_leader {

            // Wait for result
            match rx.wait_for(|val| val.is_some()).await {
                Ok(guard) => {
                    if let Some(cached) = guard.as_ref() {
                        return Ok(server.write_response(cached.clone(), "L2-HIT"));
                    }
                }
                Err(_) => {
                     // Sender dropped (Leader failed or finished without setting value?)
                     // If leader failed, we could retry or fail.
                     return Err(StatusCode::BAD_GATEWAY);
                }
            }
            return Err(StatusCode::BAD_GATEWAY);
        }

        // Leader path (we have tx_opt)
        let tx = tx_opt.unwrap();
        let result = server.fetch_and_cache(path_and_query, &cache_key, &ns).await;

        // Publish result
        if let Ok(ref cached) = result {
             let _ = tx.send(Some(cached.clone()));
        }

        // Remove inflight entry
        server.inflight.remove(&cache_key);

        match result {
            Ok(c) => Ok(server.write_response(c, "MISS")),
            Err(_) => Err(StatusCode::BAD_GATEWAY),
        }

    }


    async fn get_from_l1(&self, key: &str) -> Result<CachedResponse> {
        let value: Vec<u8> = self.l1_client.get(key).await?;
        Ok(serde_json::from_slice(&value)?)
    }

    async fn fetch_and_cache(
        &self,
        path: &str,
        key: &str,
        ns: &str,
    ) -> Result<CachedResponse> {

        let _permit = self.upstream_sem.clone().acquire_owned().await?;

        let url = format!("{}{}", self.config.cdisc.base_url, path);
        let resp = self.http_client.get(&url)
            .header("Api-Key", &self.config.cdisc.api_key)
            .send()
            .await?;

        let status = resp.status().as_u16();
        let headers = resp.headers().iter()
            .map(|(k, v)| (k.to_string(), v.to_str().unwrap_or("").to_string()))
            .collect::<HashMap<_, _>>();

        let body = resp.bytes().await?.to_vec();

        if body.len() > MAX_L2_OBJECT_SIZE {
            return Ok(CachedResponse {
                status_code: status,
                headers,
                body,
                expires_at: 0,
            });
        }

        let ttl = if status < 400{
            self.l2_ttl
        } else if status < 500 {
            NEGATIVE_CACHE_TTL_4XX
        } else {
            NEGATIVE_CACHE_TTL_OTHER
        };

        let expires_at = SystemTime::now() + ttl;
        let expires_ts = expires_at.duration_since(UNIX_EPOCH)?.as_secs();

        let cached = CachedResponse {
            status_code: status,
            headers,
            body: body.clone(),
            expires_at: expires_ts,
        };

        let serialized = serde_json::to_vec(&cached)?;

        // 1. Write to L2 synchronously (durable)
        self.storage.upsert_cache(key, &serialized, expires_at, ns).await?;

        // 2. Write to L1 asynchronously (best-effort)
        self.async_save_to_l1(key, &cached).await;

        Ok(cached)
    }


    async fn async_save_to_l1(&self, key: &str, cached: &CachedResponse) {
        if cached.body.len() > MAX_L1_OBJECT_SIZE {
            return;
        }

        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_secs();

        if cached.expires_at <= now {
            return;
        }

        let remaining = Duration::from_secs(cached.expires_at - now);
        let ttl = remaining.min(self.l1_ttl).as_secs() as i64;

        let data = match serde_json::to_vec(cached) {
            Ok(d) => d,
            Err(_) => return,
        };

        let client = self.l1_client.clone();
        let key_owned = key.to_string();

        tokio::spawn(async move {
            let _ = client
                .set::<(), _, _>(&key_owned, data, Some(Expiration::EX(ttl)), None, false)
                .await;
        });
    }



    fn write_response(&self, cached: CachedResponse, tier: &str) -> Response {
        let mut builder = Response::builder()
            .status(cached.status_code)
            .header("X-Cache-Tier", tier);

        for (k, v) in cached.headers {
            builder = builder.header(k, v);
        }

        builder.body(Body::from(cached.body)).unwrap()
    }

    pub async fn handle_health(State(server): State<Arc<ProxyServer>>) -> impl IntoResponse {
        let health_key = "system:health:v1";

        // Try L1 cache first
        if let Ok(cached) = server.get_from_l1(health_key).await {
            // cached.body is raw JSON bytes; return as-is
            return (
                StatusCode::OK,
                [("X-Health-Source", "L1-Cache")],
                cached.body.clone(),
            );
        }

        // Perform live checks
        let mut status = HealthStatus {
            status: "healthy".to_string(),
            timestamp: SystemTime::now(),
            details: HashMap::new(),
        };
        let mut is_healthy = true;

        // Check L1
        match server.l1_client.ping::<String>(None).await {
            Ok(_) => {
                status.details.insert("l1".to_string(), "healthy".to_string());
            }
            Err(e) => {
                status
                    .details
                    .insert("l1".to_string(), format!("unhealthy: {}", e));
                is_healthy = false;
            }
        };

        // Check L2
        match server.storage.ping().await {
            Ok(_) => {
                status.details.insert("l2".to_string(), "healthy".to_string());
            }
            Err(e) => {
                status
                    .details
                    .insert("l2".to_string(), format!("unhealthy: {}", e));
                is_healthy = false;
            }
        };

        if !is_healthy {
            status.status = "degraded".to_string();
        }

        let body = serde_json::to_vec(&status).unwrap_or_else(|_| b"{}".to_vec());

        (
            StatusCode::OK,
            [("X-Health-Source", "Live")],
            body,
        )
    }

    pub fn inflight_len(&self) -> usize {
        self.inflight.len()
    }

}

// Helper Functions
fn identify_namespace(path: &str) -> (String, bool) {
    let path = if path.starts_with("/api") {
        path.strip_prefix("/api").unwrap_or(path)
    } else {
        path
    };

    if !path.starts_with("/mdr/") {
        return ("".to_string(), false);
    }

    let parts: Vec<&str> = path.trim_start_matches("/mdr/").split('/').collect();
    if parts.is_empty() {
        return ("".to_string(), false);
    }

    let ns = parts[0];
    let valid = matches!(
        ns,
        "adam" | "sdtm" | "sdtmig" | "sendig" | "cdash" | "cdashig" | "qrs" | "ct" | "integrated" | "about" | "products"
    );

    (ns.to_string(), valid)
}


pub fn parse_ttl(ttl: &str) -> Result<Duration> {
    if let Some(stripped) = ttl.strip_suffix('y') {
        let n: u64 = stripped.parse()?;
        Ok(Duration::from_secs(n * 365 * 24 * 3600))
    } else if let Some(stripped) = ttl.strip_suffix('w') {
        let n: u64 = stripped.parse()?;
        Ok(Duration::from_secs(n * 7 * 24 * 3600))
    } else if let Some(stripped) = ttl.strip_suffix('d') {
        let n: u64 = stripped.parse()?;
        Ok(Duration::from_secs(n * 24 * 3600))
    } else {
        Ok(humantime::parse_duration(ttl)?)
    }
}

pub fn validate_config(config: &AppConfig) -> Result<()> {
    if config.server.port == 0 {
        return Err(anyhow!("Invalid port"));
    }
    if config.cdisc.base_url.is_empty() || config.cdisc.api_key.is_empty() {
        return Err(anyhow!("CDISC config required"));
    }
    if config.cache.l1.address.is_empty() {
        return Err(anyhow!("L1 address required"));
    }
    if config.cache.l2.storage_path.is_none() && config.cache.l2.postgres_dsn.is_none() {
        return Err(anyhow!("L2 storage required"));
    }
    Ok(())
}

