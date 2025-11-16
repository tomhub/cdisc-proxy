package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/valkey-io/valkey-go"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		Port      int      `yaml:"port"`
		Listen    []string `yaml:"listen"`   // IP addresses to listen on (supports IPv4 and IPv6)
		AuthKey   string   `yaml:"auth_key"` // API key for protecting the refresh endpoint
	} `yaml:"server"`
	CDISC struct {
		BaseURL string `yaml:"base_url"`
		APIKey  string `yaml:"api_key"`
	} `yaml:"cdisc"`
	Valkey struct {
		Address  string `yaml:"address"`
		Password string `yaml:"password"`
		DB       int    `yaml:"db"`
	} `yaml:"valkey"`
	Cache struct {
		TTL string `yaml:"ttl"` // Cache duration (e.g., "24h", "7d", "1y")
	} `yaml:"cache"`
}

type ProxyServer struct {
	config        Config
	valkeyClient  valkey.Client
	httpClient    *http.Client
	ctx           context.Context
	cacheTTL      time.Duration
}

func loadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	return &config, nil
}

func parseDuration(ttlStr string) (time.Duration, error) {
	// Support custom duration formats like "1y" for 1 year, "7d" for 7 days
	if ttlStr == "" {
		return 365 * 24 * time.Hour, nil // Default to 1 year
	}
	
	// Check for custom formats
	if len(ttlStr) >= 2 {
		unit := ttlStr[len(ttlStr)-1:]
		valueStr := ttlStr[:len(ttlStr)-1]
		
		var value int
		if _, err := fmt.Sscanf(valueStr, "%d", &value); err == nil {
			switch unit {
			case "y":
				return time.Duration(value) * 365 * 24 * time.Hour, nil
			case "d":
				return time.Duration(value) * 24 * time.Hour, nil
			case "w":
				return time.Duration(value) * 7 * 24 * time.Hour, nil
			}
		}
	}
	
	// Try standard Go duration format (e.g., "24h", "30m")
	return time.ParseDuration(ttlStr)
}

func NewProxyServer(config Config) (*ProxyServer, error) {
	ctx := context.Background()

	// Parse cache TTL
	cacheTTL, err := parseDuration(config.Cache.TTL)
	if err != nil {
		return nil, fmt.Errorf("invalid cache TTL: %w", err)
	}
	log.Printf("Cache TTL set to: %v", cacheTTL)

	// Initialize Valkey client
	clientOpts := valkey.ClientOption{
		InitAddress: []string{config.Valkey.Address},
	}
	
	if config.Valkey.Password != "" {
		clientOpts.Password = config.Valkey.Password
	}
	
	if config.Valkey.DB != 0 {
		clientOpts.SelectDB = config.Valkey.DB
	}

	valkeyClient, err := valkey.NewClient(clientOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to create Valkey client: %w", err)
	}

	// Test Valkey connection
	if err := valkeyClient.Do(ctx, valkeyClient.B().Ping().Build()).Error(); err != nil {
		valkeyClient.Close()
		return nil, fmt.Errorf("failed to connect to Valkey: %w", err)
	}

	return &ProxyServer{
		config:        config,
		valkeyClient:  valkeyClient,
		httpClient:    &http.Client{Timeout: 30 * time.Second},
		ctx:           ctx,
		cacheTTL:      cacheTTL,
	}, nil
}

func (ps *ProxyServer) getCacheKey(path string) string {
	return fmt.Sprintf("cdisc:cache:%s", path)
}

func (ps *ProxyServer) fetchFromCDISC(path string) ([]byte, int, error) {
	url := ps.config.CDISC.BaseURL + path
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("api-key", ps.config.CDISC.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := ps.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}

	return body, resp.StatusCode, nil
}

func (ps *ProxyServer) cacheResponse(key string, data []byte) error {
	cmd := ps.valkeyClient.B().Set().Key(key).Value(string(data)).Ex(ps.cacheTTL).Build()
	return ps.valkeyClient.Do(ps.ctx, cmd).Error()
}

func (ps *ProxyServer) getFromCache(key string) ([]byte, bool, error) {
	cmd := ps.valkeyClient.B().Get().Key(key).Build()
	result := ps.valkeyClient.Do(ps.ctx, cmd)
	
	if err := result.Error(); err != nil {
		// Check if key doesn't exist
		if valkey.IsValkeyNil(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	
	data, err := result.AsBytes()
	if err != nil {
		return nil, false, err
	}
	
	return data, true, nil
}

func (ps *ProxyServer) authenticate(r *http.Request) bool {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return false
	}

	// Support "Bearer <token>" format
	token := strings.TrimPrefix(authHeader, "Bearer ")
	token = strings.TrimSpace(token)

	// Use constant-time comparison to prevent timing attacks
	return subtle.ConstantTimeCompare([]byte(token), []byte(ps.config.Server.AuthKey)) == 1
}

// Regular proxy endpoint - serves from cache or fetches and caches
func (ps *ProxyServer) handleProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract the CDISC API path
	path := strings.TrimPrefix(r.URL.Path, "/api")
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}

	cacheKey := ps.getCacheKey(path)

	// Try to get from cache first
	cachedData, found, err := ps.getFromCache(cacheKey)
	if err != nil {
		log.Printf("Cache error: %v", err)
	}

	if found {
		log.Printf("Cache hit for: %s", path)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "HIT")
		w.Write(cachedData)
		return
	}

	// Cache miss - fetch from CDISC
	log.Printf("Cache miss for: %s", path)
	data, statusCode, err := ps.fetchFromCDISC(path)
	if err != nil {
		log.Printf("Error fetching from CDISC: %v", err)
		http.Error(w, "Error fetching from CDISC API", http.StatusBadGateway)
		return
	}

	// Cache only successful responses
	if statusCode == http.StatusOK {
		if err := ps.cacheResponse(cacheKey, data); err != nil {
			log.Printf("Error caching response: %v", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(statusCode)
	w.Write(data)
}

// Refresh endpoint - forces fetch from CDISC and updates cache
func (ps *ProxyServer) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check authentication
	if !ps.authenticate(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="CDISC Proxy"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Get path from request body
	var request struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if request.Path == "" {
		http.Error(w, "Path is required", http.StatusBadRequest)
		return
	}

	log.Printf("Refresh requested for: %s", request.Path)

	// Fetch from CDISC
	data, statusCode, err := ps.fetchFromCDISC(request.Path)
	if err != nil {
		log.Printf("Error fetching from CDISC: %v", err)
		http.Error(w, "Error fetching from CDISC API", http.StatusBadGateway)
		return
	}

	// Always cache the response, regardless of whether it existed before
	cacheKey := ps.getCacheKey(request.Path)
	if statusCode == http.StatusOK {
		if err := ps.cacheResponse(cacheKey, data); err != nil {
			log.Printf("Error caching response: %v", err)
		}
	}

	response := map[string]interface{}{
		"status":       "success",
		"cached":       statusCode == http.StatusOK,
		"status_code":  statusCode,
		"content_size": len(data),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// Health check endpoint
func (ps *ProxyServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Check Valkey connection
	cmd := ps.valkeyClient.B().Ping().Build()
	if err := ps.valkeyClient.Do(ps.ctx, cmd).Error(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"status": "unhealthy",
			"error":  "Valkey connection failed",
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "healthy",
	})
}

func (ps *ProxyServer) Close() {
	ps.valkeyClient.Close()
}

func main() {
	// Determine config file path
	var configFile string
	
	if len(os.Args) > 1 {
		// Command line argument takes precedence
		configFile = os.Args[1]
	} else {
		// Default to /etc/conf.d/cdisc-proxy.conf
		configFile = "/etc/conf.d/cdisc-proxy.conf"
	}

	log.Printf("Loading configuration from: %s", configFile)
	config, err := loadConfig(configFile)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	server, err := NewProxyServer(*config)
	if err != nil {
		log.Fatalf("Failed to initialize server: %v", err)
	}
	defer server.Close()

	http.HandleFunc("/api/", server.handleProxy)
	http.HandleFunc("/refresh", server.handleRefresh)
	http.HandleFunc("/health", server.handleHealth)

	// Default to listening on all interfaces if no listen addresses specified
	listenAddrs := config.Server.Listen
	if len(listenAddrs) == 0 {
		listenAddrs = []string{"0.0.0.0"}
	}

	log.Printf("Starting CDISC Library API Proxy on port %d", config.Server.Port)
	log.Printf("Listening on: %v", listenAddrs)
	log.Printf("Proxy endpoint: /api/*")
	log.Printf("Refresh endpoint: POST /refresh (requires authentication)")
	log.Printf("Health check: /health")

	// Channel to collect errors from multiple listeners
	errChan := make(chan error, len(listenAddrs))

	// Start HTTP server on each specified listen address
	for _, addr := range listenAddrs {
		var listenAddr string
		
		// IPv6 addresses need to be wrapped in brackets
		if strings.Contains(addr, ":") {
			listenAddr = fmt.Sprintf("[%s]:%d", addr, config.Server.Port)
		} else {
			listenAddr = fmt.Sprintf("%s:%d", addr, config.Server.Port)
		}
		
		go func(address string) {
			log.Printf("Starting listener on %s", address)
			if err := http.ListenAndServe(address, nil); err != nil {
				errChan <- fmt.Errorf("listener %s failed: %w", address, err)
			}
		}(listenAddr)
	}

	// Wait for any listener to fail
	err = <-errChan
	log.Fatalf("Server failed: %v", err)
}
