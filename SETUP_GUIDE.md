# CDISC API Cache Setup Guide

## Overview

This setup provides persistent caching for the CDISC Library API with intelligent cache invalidation based on product update dates. **Varnish acts as a proxy that injects your API key into upstream requests**, so clients don't need to know or provide the key.

## Security Model

- **API Key Storage**: Your CDISC API key is stored securely in Varnish configuration
- **Client Access**: Clients connect to Varnish without needing the API key
- **Upstream Requests**: Varnish automatically injects the API key when contacting CDISC API
- **Key Protection**: API keys are never exposed to clients in responses

## Components

1. **Varnish Configuration** - Handles HTTP caching with persistent storage
2. **Rust Invalidator** - Monitors the `/api/mdr/lastupdated` endpoint and invalidates cache when products are updated

## Varnish Setup

### 1. Install Varnish

**Ubuntu/Debian:**
```bash
sudo apt-get update
sudo apt-get install varnish
```

**RHEL/CentOS:**
```bash
sudo yum install varnish
```

**Alpine Linux:**
```bash
sudo apk add varnish
```

**macOS:**
```bash
brew install varnish
```

### 2. Configure Persistent Storage

**Ubuntu/Debian/RHEL/CentOS:**

Edit `/etc/varnish/varnish.params` and add persistent storage:

```bash
VARNISH_STORAGE="persistent,/var/lib/varnish/storage.bin,50G"
```

**Alpine Linux:**

Edit `/etc/varnish/varnishd` or create `/etc/conf.d/varnishd`:

```bash
# Varnish configuration for Alpine
VARNISHD_OPTS="-s persistent,/var/lib/varnish/storage.bin,50G \
               -a :6081 \
               -T localhost:6082 \
               -f /etc/varnish/default.vcl \
               -p feature=+http2"
```

This creates a 50GB persistent storage file. Adjust size based on your needs.

### 3. Configure API Key

**Create the API key configuration file:**

```bash
sudo mkdir -p /etc/varnish
sudo touch /etc/varnish/api_key.vcl
sudo chmod 600 /etc/varnish/api_key.vcl
```

**Option 1: Using Environment Variable (Recommended)**

Copy the provided `api_key.vcl` file to `/etc/varnish/api_key.vcl`, then set the environment variable:

```bash
# For systemd (Ubuntu/Debian/RHEL/CentOS)
sudo mkdir -p /etc/systemd/system/varnish.service.d
sudo tee /etc/systemd/system/varnish.service.d/override.conf << EOF
[Service]
Environment="CDISC_API_KEY=your-actual-api-key-here"
EOF
sudo systemctl daemon-reload
```

```bash
# For OpenRC (Alpine Linux)
echo 'export CDISC_API_KEY="your-actual-api-key-here"' | sudo tee -a /etc/conf.d/varnishd
```

**Option 2: Hardcode in VCL (Less Secure)**

Edit `/etc/varnish/api_key.vcl` and replace the content:

```vcl
C{
    const char *CDISC_API_KEY = "your-actual-api-key-here";
}C
```

**Security Note:** Ensure proper file permissions:
```bash
sudo chown root:varnish /etc/varnish/api_key.vcl
sudo chmod 640 /etc/varnish/api_key.vcl
```

### 3. Deploy VCL Configuration

Copy both VCL configuration files:

```bash
sudo cp default.vcl /etc/varnish/default.vcl
sudo cp api_key.vcl /etc/varnish/api_key.vcl
sudo chown root:varnish /etc/varnish/*.vcl
sudo chmod 640 /etc/varnish/api_key.vcl
sudo chmod 644 /etc/varnish/default.vcl
```

### 4. Start Varnish

**Ubuntu/Debian/RHEL/CentOS (systemd):**
```bash
sudo systemctl enable varnish
sudo systemctl start varnish
sudo systemctl status varnish
```

**Alpine Linux (OpenRC):**
```bash
sudo rc-update add varnishd default
sudo rc-service varnishd start
sudo rc-service varnishd status
```

### 5. Verify Varnish is Running

```bash
# Check if Varnish is listening
curl -I http://localhost:6081/api/mdr/products

# Verify cache headers (should see X-Cache header)
curl -I http://localhost:6081/api/mdr/products | grep X-Cache

# IMPORTANT: Verify API key is NOT exposed to clients
curl -I http://localhost:6081/api/mdr/products | grep -i "api-key"
# This should return nothing - the key should be hidden
```

## Rust Invalidator Setup

### 1. Install Rust

**Ubuntu/Debian/RHEL/CentOS/macOS:**
```bash
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
source $HOME/.cargo/env
```

**Alpine Linux:**
```bash
# Install Rust and build dependencies
sudo apk add rust cargo musl-dev openssl-dev

# Or use rustup
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
source $HOME/.cargo/env
```

### 2. Build the Invalidator

```bash
# Create project directory
mkdir cdisc-cache-invalidator
cd cdisc-cache-invalidator

# Copy the Cargo.toml and main.rs files
# Then build
cargo build --release
```

### 3. Configure Environment Variables

Create a `.env` file or systemd service file:

```bash
# Invalidator connects to Varnish cache, not directly to CDISC API
VARNISH_URL=http://localhost:6081
STATE_FILE=/var/lib/cdisc-invalidator/state.json
CHECK_INTERVAL_SECS=300
```

**Important:** The invalidator queries `/api/mdr/lastupdated` **through the Varnish cache** (not directly from CDISC API). This ensures:
- The invalidator benefits from caching
- Reduced load on CDISC API
- Consistent behavior between invalidator and clients

### 4. Create Service File

**Systemd (Ubuntu/Debian/RHEL/CentOS):**

Create `/etc/systemd/system/cdisc-invalidator.service`:

```ini
[Unit]
Description=CDISC Cache Invalidator
After=network.target varnish.service
Requires=varnish.service

[Service]
Type=simple
User=varnish
Group=varnish
WorkingDirectory=/opt/cdisc-invalidator
ExecStart=/opt/cdisc-invalidator/cdisc-cache-invalidator
Restart=always
RestartSec=10

Environment="VARNISH_URL=http://localhost:6081"
Environment="STATE_FILE=/var/lib/cdisc-invalidator/state.json"
Environment="CHECK_INTERVAL_SECS=300"

[Install]
WantedBy=multi-user.target
```

**OpenRC (Alpine Linux):**

Create `/etc/init.d/cdisc-invalidator`:

```bash
#!/sbin/openrc-run

name="CDISC Cache Invalidator"
description="Monitors CDISC API and invalidates Varnish cache"

command="/opt/cdisc-invalidator/cdisc-cache-invalidator"
command_user="varnish:varnish"
command_background=true
pidfile="/run/${RC_SVCNAME}.pid"
directory="/opt/cdisc-invalidator"

# Environment variables
export VARNISH_URL="http://localhost:6081"
export STATE_FILE="/var/lib/cdisc-invalidator/state.json"
export CHECK_INTERVAL_SECS="300"

depend() {
    need net varnishd
    after firewall
}

start_pre() {
    checkpath --directory --owner varnish:varnish --mode 0755 \
        /var/lib/cdisc-invalidator
}
```

Make it executable:
```bash
sudo chmod +x /etc/init.d/cdisc-invalidator
```

### 5. Deploy and Start

**Common steps:**
```bash
# Create directories
sudo mkdir -p /opt/cdisc-invalidator
sudo mkdir -p /var/lib/cdisc-invalidator

# Copy binary
sudo cp target/release/cdisc-cache-invalidator /opt/cdisc-invalidator/

# Set permissions
sudo chown -R varnish:varnish /opt/cdisc-invalidator
sudo chown -R varnish:varnish /var/lib/cdisc-invalidator
```

**Systemd (Ubuntu/Debian/RHEL/CentOS):**
```bash
sudo systemctl daemon-reload
sudo systemctl enable cdisc-invalidator
sudo systemctl start cdisc-invalidator
sudo systemctl status cdisc-invalidator
```

**OpenRC (Alpine Linux):**
```bash
sudo rc-update add cdisc-invalidator default
sudo rc-service cdisc-invalidator start
sudo rc-service cdisc-invalidator status
```

## Docker Deployment

### Quick Start with Docker Compose

1. **Create project directory:**

```bash
mkdir cdisc-cache && cd cdisc-cache
```

2. **Create required files:**

Save the following files in your project directory:
- `Dockerfile` - The invalidator container definition
- `docker-compose.yml` - Service orchestration
- `default.vcl` - Varnish configuration
- `api_key.vcl` - API key configuration
- `nginx.conf` - (Optional) SSL termination config
- `.env` - Environment variables (copy from `.env.example`)

3. **Configure API Key:**

```bash
# Copy the environment template
cp .env.example .env

# Edit .env and add your CDISC API key
nano .env
# Set: CDISC_API_KEY=your-actual-api-key-here
```

**IMPORTANT:** Never commit `.env` to version control! It's already in `.gitignore`.

4. **Build and start services:**

```bash
# Build the invalidator image
docker-compose build

# Start all services
docker-compose up -d

# View logs
docker-compose logs -f

# Check status
docker-compose ps
```

5. **Verify deployment:**

```bash
# Test cache
curl -I http://localhost:6081/api/mdr/products

# Check cache headers
curl -I http://localhost:6081/api/mdr/products | grep X-Cache

# IMPORTANT: Verify API key is NOT exposed
curl -I http://localhost:6081/api/mdr/products | grep -i "api-key"
# This should return nothing

# View invalidator logs
docker-compose logs invalidator
```

### Docker Build Only (Without Compose)

**Build the invalidator:**

```bash
docker build -t cdisc-cache-invalidator:latest .
```

**Run Varnish:**

```bash
docker run -d \
  --name cdisc-varnish \
  -p 6081:6081 \
  -p 6082:6082 \
  -v $(pwd)/default.vcl:/etc/varnish/default.vcl:ro \
  -v $(pwd)/api_key.vcl:/etc/varnish/api_key.vcl:ro \
  -v varnish-data:/var/lib/varnish \
  -e CDISC_API_KEY="your-actual-api-key-here" \
  varnish:7.4-alpine \
  -F -f /etc/varnish/default.vcl \
  -s persistent,/var/lib/varnish/storage.bin,50G \
  -a :6081 -T :6082
```

**Run invalidator:**

```bash
docker run -d \
  --name cdisc-invalidator \
  --network container:cdisc-varnish \
  -v invalidator-state:/var/lib/cdisc-invalidator \
  -e CDISC_API_URL=https://library.cdisc.org \
  -e VARNISH_URL=http://localhost:6081 \
  -e STATE_FILE=/var/lib/cdisc-invalidator/state.json \
  -e CHECK_INTERVAL_SECS=300 \
  cdisc-cache-invalidator:latest
```

### Alpine Linux Specific Docker Setup

**On Alpine host, install Docker:**

```bash
# Install Docker
sudo apk add docker docker-compose

# Start Docker service
sudo rc-update add docker boot
sudo rc-service docker start

# Add user to docker group
sudo addgroup $USER docker
```

**Enable BuildKit for faster builds:**

```bash
export DOCKER_BUILDKIT=1
export COMPOSE_DOCKER_CLI_BUILD=1
```

### Docker Management

**View logs:**

```bash
# All services
docker-compose logs -f

# Specific service
docker-compose logs -f invalidator
docker-compose logs -f varnish
```

**Restart services:**

```bash
# Restart all
docker-compose restart

# Restart specific service
docker-compose restart invalidator
```

**Update and rebuild:**

```bash
# Pull latest base images
docker-compose pull

# Rebuild invalidator
docker-compose build --no-cache invalidator

# Restart with new image
docker-compose up -d
```

**Stop and clean up:**

```bash
# Stop all services
docker-compose down

# Stop and remove volumes (WARNING: deletes cache!)
docker-compose down -v

# Remove all containers and images
docker-compose down --rmi all
```

### Docker Health Checks

**Check container health:**

```bash
# View health status
docker-compose ps

# Inspect health details
docker inspect --format='{{json .State.Health}}' cdisc-varnish | jq
docker inspect --format='{{json .State.Health}}' cdisc-invalidator | jq
```

### Persistent Data

Docker volumes are used for persistent storage:

```bash
# List volumes
docker volume ls

# Inspect volume
docker volume inspect cdisc-cache_varnish-storage

# Backup volumes
docker run --rm \
  -v cdisc-cache_varnish-storage:/source \
  -v $(pwd):/backup \
  alpine tar czf /backup/varnish-backup.tar.gz -C /source .

# Restore volumes
docker run --rm \
  -v cdisc-cache_varnish-storage:/target \
  -v $(pwd):/backup \
  alpine tar xzf /backup/varnish-backup.tar.gz -C /target
```

### Configuration Updates

**Update Varnish VCL without downtime:**

```bash
# Edit default.vcl
vim default.vcl

# Reload Varnish configuration
docker-compose exec varnish varnishadm vcl.load newconfig /etc/varnish/default.vcl
docker-compose exec varnish varnishadm vcl.use newconfig
```

**Update environment variables:**

Edit `docker-compose.yml`, then:

```bash
docker-compose up -d --force-recreate invalidator
```

## How It Works

### Architecture Flow

```
Invalidator (every 5 min)
    ↓
Queries: http://varnish:6081/api/mdr/lastupdated
    ↓
Varnish Cache (5 min TTL for lastupdated)
    ↓
If cache MISS → CDISC API (with injected API key)
    ↓
Response cached and returned
    ↓
Invalidator compares with saved state
    ↓
If changes detected → BAN cache for affected product groups
    ↓
State saved to disk (survives reboots)
```

### State Persistence

The invalidator maintains a state file that persists across reboots:

**State File Location:** `/var/lib/cdisc-invalidator/state.json`

**Example state file:**
```json
{
  "data-analysis": "2024-07-11",
  "data-collection": "2024-07-11",
  "data-tabulation": "2024-11-15",
  "integrated": "2024-10-08",
  "overall": "2025-09-30",
  "qrs": "2025-04-29",
  "schemas": "2024-04-11",
  "terminology": "2025-09-30"
}
```

**On startup:**
1. Invalidator loads previous state from disk
2. If state file exists, it shows the last known update dates
3. On first run (no state file), it initializes state with current data
4. State is saved atomically (write to temp file, then rename) to prevent corruption
5. State persists across container restarts, host reboots, and service restarts

**Benefits:**
- No unnecessary cache invalidations after restart
- Reliable change detection across system reboots
- Atomic writes prevent state file corruption
- Human-readable JSON format for debugging

### Caching Strategy

1. **200 OK responses**: Cached for 6 years with persistent storage
2. **Error responses (4xx, 5xx)**: Cached for 10 minutes
3. **Other responses**: Cached for 5 minutes
4. **lastupdated endpoint**: Cached for 5 minutes (configurable via `X-Lastupdated-TTL` header)

**Configure lastupdated cache time:**

```bash
# Cache for 10 minutes instead of 5
curl -H "X-Lastupdated-TTL: 600" http://localhost:6081/api/mdr/lastupdated

# Cache for 1 minute
curl -H "X-Lastupdated-TTL: 60" http://localhost:6081/api/mdr/lastupdated
```

### Cache Invalidation

The Rust invalidator:

1. Polls `/api/mdr/lastupdated` **through Varnish cache** every 5 minutes (configurable)
2. Benefits from the 5-minute cache on lastupdated endpoint
3. Loads previous state from disk on startup (survives reboots)
4. Compares current dates with saved state
5. Issues BAN requests to Varnish for changed product groups
6. Saves new state atomically to disk
7. State persists across restarts and reboots

**Product Group Mappings:**

- `data-analysis` → `/api/mdr/products/DataAnalysis`, `/mdr/adam/*`
- `data-collection` → `/api/mdr/products/DataCollection`, `/mdr/cdash/*`
- `data-tabulation` → `/api/mdr/products/DataTabulation`, `/mdr/sdtm/*`, `/mdr/sendig/*`
- `integrated` → `/api/mdr/products/Integrated`, `/mdr/integrated/*`
- `qrs` → `/api/mdr/products/QrsInstrument`, `/mdr/qrs/*`
- `terminology` → `/api/mdr/products/Terminology`, `/mdr/ct/*`

## Testing and Verification

### Test State Persistence

Use the provided test script to verify everything is working correctly:

```bash
# Make script executable
chmod +x test_persistence.sh

# Run tests
./test_persistence.sh
```

The test script verifies:
- ✓ Service is running
- ✓ State file exists and is valid
- ✓ State persists across restarts
- ✓ State content unchanged after restart
- ✓ All required fields present
- ✓ Invalidator queries through cache
- ✓ Atomic writes working correctly

### Manual Testing

**Test 1: Verify cache is working**
```bash
# First request (should be MISS)
curl -I http://localhost:6081/api/mdr/products
# Look for: X-Cache: MISS

# Second request (should be HIT)
curl -I http://localhost:6081/api/mdr/products
# Look for: X-Cache: HIT
```

**Test 2: Verify API key is hidden**
```bash
# Should return nothing
curl -I http://localhost:6081/api/mdr/products | grep -i "api-key"
```

**Test 3: Verify state persistence after reboot**
```bash
# 1. Check current state
cat /var/lib/cdisc-invalidator/state.json  # or docker-compose exec...

# 2. Reboot system or restart services
sudo reboot  # or: sudo systemctl restart varnish cdisc-invalidator

# 3. After restart, check logs
sudo journalctl -u cdisc-invalidator -n 30 | grep "Loaded previous state"
# Should show: "Loaded previous state from disk"

# 4. Verify state is unchanged
cat /var/lib/cdisc-invalidator/state.json
```

**Test 4: Verify invalidator uses cache**
```bash
# Watch invalidator logs
sudo journalctl -u cdisc-invalidator -f  # or: docker-compose logs -f invalidator

# Look for:
# - "Fetching lastupdated from cache: http://localhost:6081/api/mdr/lastupdated"
# - "Cache status: HIT" or "Cache status: MISS"
```

**Test 5: Simulate product update**
```bash
# Manually trigger a cache invalidation to test the flow
curl -X BAN -H "X-Ban-Product: data-analysis" http://localhost:6081/

# Check logs for invalidation
sudo journalctl -u cdisc-invalidator -n 20
```

## Monitoring

### Check Cache Status

```bash
# View cache stats
varnishstat

# Monitor real-time logs
varnishlog

# Check specific requests
varnishlog -q "ReqURL ~ '/api/mdr/products'"
```

### Check Invalidator Logs

**Systemd:**
```bash
# View logs
sudo journalctl -u cdisc-invalidator -f

# Check for invalidation events
sudo journalctl -u cdisc-invalidator | grep "Invalidating cache"
```

**OpenRC (Alpine):**
```bash
# View logs
sudo tail -f /var/log/cdisc-invalidator.log

# Check logs with logread (if using syslog)
sudo logread -f | grep cdisc-invalidator
```

**Docker:**
```bash
# View logs
docker-compose logs -f invalidator

# Check for invalidation events
docker-compose logs invalidator | grep "Invalidating cache"
```

### View Current State

**Native installation:**
```bash
cat /var/lib/cdisc-invalidator/state.json
```

**Docker:**
```bash
docker-compose exec invalidator cat /var/lib/cdisc-invalidator/state.json
```

**Example output after reboot:**
```
CDISC Cache Invalidator starting...
Varnish URL: http://localhost:6081
Check interval: 300s
State file: /var/lib/cdisc-invalidator/state.json
Loaded previous state from disk:
  data-analysis: 2024-07-11
  data-collection: 2024-07-11
  data-tabulation: 2024-11-15
  integrated: 2024-10-08
  qrs: 2025-04-29
  terminology: 2025-09-30
---
```

## Manual Cache Invalidation

### Purge Specific URL

```bash
curl -X PURGE http://localhost:6081/api/mdr/products/DataAnalysis
```

### Ban Product Group

```bash
curl -X BAN \
  -H "X-Ban-Product: data-analysis" \
  http://localhost:6081/
```

### Ban All Cache

**Native installation:**
```bash
varnishadm "ban req.url ~ ."
```

**Docker:**
```bash
docker-compose exec varnish varnishadm "ban req.url ~ ."
```

## Troubleshooting

### Cache Not Working

1. **Check Varnish is running:**
   - Systemd: `sudo systemctl status varnish`
   - OpenRC: `sudo rc-service varnishd status`
   - Docker: `docker-compose ps varnish`

2. **Check VCL syntax:**
   - Native: `sudo varnishd -C -f /etc/varnish/default.vcl`
   - Docker: `docker-compose exec varnish varnishd -C -f /etc/varnish/default.vcl`

3. **Verify storage:**
   - Native: `ls -lh /var/lib/varnish/storage.bin`
   - Docker: `docker volume inspect cdisc-cache_varnish-storage`

4. **Check headers:**
   ```bash
   curl -I http://localhost:6081/api/mdr/products
   ```

### Invalidator Not Running

1. **Check service status:**
   - Systemd: `sudo systemctl status cdisc-invalidator`
   - OpenRC: `sudo rc-service cdisc-invalidator status`
   - Docker: `docker-compose ps invalidator`

2. **View logs:**
   - Systemd: `sudo journalctl -u cdisc-invalidator -n 50`
   - OpenRC: `sudo tail -n 50 /var/log/cdisc-invalidator.log`
   - Docker: `docker-compose logs --tail=50 invalidator`

3. **Test API connectivity:**
   ```bash
   # Should go through Varnish cache
   curl http://localhost:6081/api/mdr/lastupdated
   ```

4. **Verify state file:**
   - Native: `ls -l /var/lib/cdisc-invalidator/`
   - Docker: `docker-compose exec invalidator ls -l /var/lib/cdisc-invalidator/`

5. **Test state persistence:**
   ```bash
   # Restart the service
   sudo systemctl restart cdisc-invalidator  # or: docker-compose restart invalidator
   
   # Check logs - should show "Loaded previous state from disk"
   sudo journalctl -u cdisc-invalidator -n 20  # or: docker-compose logs invalidator
   ```

### Storage Full

**Native installation:**
```bash
# Check storage usage
du -sh /var/lib/varnish/

# Clear cache (recreates storage)
sudo systemctl stop varnish  # or: sudo rc-service varnishd stop
sudo rm /var/lib/varnish/storage.bin
sudo systemctl start varnish  # or: sudo rc-service varnishd start
```

**Docker:**
```bash
# Check volume usage
docker system df -v | grep varnish

# Clear cache (WARNING: deletes all cached data)
docker-compose down
docker volume rm cdisc-cache_varnish-storage
docker-compose up -d
```

### Docker-Specific Issues

**Container won't start:**
```bash
# Check logs
docker-compose logs varnish
docker-compose logs invalidator

# Check resource usage
docker stats

# Rebuild containers
docker-compose build --no-cache
docker-compose up -d
```

**Network issues:**
```bash
# Verify network connectivity
docker-compose exec invalidator ping -c 3 varnish
docker-compose exec varnish ping -c 3 invalidator

# Check network configuration
docker network inspect cdisc-cache_cdisc-network
```

**Permission issues in containers:**
```bash
# Check file ownership
docker-compose exec invalidator ls -la /var/lib/cdisc-invalidator/
docker-compose exec varnish ls -la /var/lib/varnish/

# Fix permissions
docker-compose exec --user root invalidator chown -R cdisc:cdisc /var/lib/cdisc-invalidator
```

## Performance Tuning

### Varnish Parameters

Edit `/etc/varnish/varnish.params`:

```bash
# Increase memory for better performance
VARNISH_STORAGE="persistent,/var/lib/varnish/storage.bin,100G"

# Adjust thread pool
DAEMON_OPTS="-p thread_pool_min=100 \
             -p thread_pool_max=1000 \
             -p thread_pool_timeout=120"
```

### Invalidator Frequency

Adjust check interval based on update frequency:

```bash
# Check every 15 minutes
CHECK_INTERVAL_SECS=900

# Check every hour
CHECK_INTERVAL_SECS=3600
```

## Security Considerations

1. **Protect API Key Files**:
   ```bash
   # Set restrictive permissions on API key file
   sudo chmod 600 /etc/varnish/api_key.vcl
   sudo chown root:varnish /etc/varnish/api_key.vcl
   ```

2. **Never Commit API Keys**: 
   - Add `api_key.vcl` and `.env` to `.gitignore`
   - Use environment variables in production
   - Rotate API keys periodically

3. **Verify Key is Hidden**:
   ```bash
   # This should return NOTHING (no api-key header)
   curl -I http://localhost:6081/api/mdr/products | grep -i "api-key"
   ```

4. **Restrict PURGE/BAN access**: Update the `purge_acl` in VCL to only allow trusted IPs

5. **Use HTTPS**: Configure SSL termination in front of Varnish (see nginx.conf)

6. **Run as non-root**: Both Varnish and invalidator should run as dedicated users

7. **Firewall**: Restrict access to Varnish management port (6082)

8. **Docker Security**:
   ```bash
   # Protect .env file
   chmod 600 .env
   
   # Use Docker secrets for production
   # See: https://docs.docker.com/engine/swarm/secrets/
   ```

## Backup and Recovery

### Backup State File

The state file is critical for preventing unnecessary cache invalidations after restarts.

```bash
# Native installation
sudo cp /var/lib/cdisc-invalidator/state.json \
       /var/lib/cdisc-invalidator/state.json.backup

# Restore state
sudo cp /var/lib/cdisc-invalidator/state.json.backup \
       /var/lib/cdisc-invalidator/state.json

# Docker
docker-compose exec invalidator cp /var/lib/cdisc-invalidator/state.json \
       /var/lib/cdisc-invalidator/state.json.backup
```

### Backup Cache

The persistent storage can be backed up while Varnish is running:

```bash
sudo cp /var/lib/varnish/storage.bin /backup/varnish-storage-$(date +%Y%m%d).bin
```

### Reboot Behavior

**After system reboot:**

1. **Varnish starts** and loads persistent cache from disk
   - All cached responses remain available
   - No warm-up period needed

2. **Invalidator starts** and loads state from disk
   - Reads `/var/lib/cdisc-invalidator/state.json`
   - Compares with current data from cache
   - Only invalidates if actual changes detected

3. **First check after reboot:**
   ```
   [2025-12-20 10:15:30] Loaded previous state from disk:
     data-analysis: 2024-07-11
     ...
   [2025-12-20 10:15:31] Fetched last updated data
     Cache status: HIT
   [2025-12-20 10:15:31] No changes detected
   ```

**If state file is lost:**

The invalidator will initialize a new state file without invalidating the cache:

```
No previous state found - will initialize on first fetch
Initializing state with current data
Initial state saved
```

**To manually reset state** (force re-check):

```bash
# Stop invalidator
sudo systemctl stop cdisc-invalidator  # or: docker-compose stop invalidator

# Delete state file
sudo rm /var/lib/cdisc-invalidator/state.json

# Start invalidator (will create new state)
sudo systemctl start cdisc-invalidator  # or: docker-compose start invalidator
```