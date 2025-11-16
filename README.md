# CDISC Library API Proxy

<div align="center">

![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)
![License](https://img.shields.io/badge/license-MIT-blue.svg)
![Platform](https://img.shields.io/badge/platform-Linux%20%7C%20Alpine-lightgrey)

A high-performance caching proxy server for the [CDISC Library API](https://www.cdisc.org/cdisc-library) with persistent storage in Valkey/Redis.

[Features](#features) • [Quick Start](#quick-start) • [Configuration](#configuration) • [API Usage](#api-usage) • [Deployment](#deployment)

</div>

---

## 📋 Table of Contents

- [Features](#features)
- [Why Use This Proxy?](#why-use-this-proxy)
- [Prerequisites](#prerequisites)
- [Quick Start](#quick-start)
- [Configuration](#configuration)
- [API Usage](#api-usage)
- [Cache Management](#cache-management)
- [Deployment](#deployment)
  - [Alpine Linux (OpenRC)](#alpine-linux-openrc)
  - [Systemd](#systemd)
  - [Docker](#docker)
- [Monitoring](#monitoring)
- [Security](#security)
- [Troubleshooting](#troubleshooting)
- [Contributing](#contributing)
- [License](#license)

---

## ✨ Features

- 🚀 **High Performance** - Built with Go for speed and efficiency
- 💾 **Persistent Caching** - Stores responses in Valkey/Redis with configurable TTL
- 🔄 **Force Refresh** - Authenticated endpoint to update cached data on demand
- 🌐 **Multi-Interface Support** - Listen on multiple IPv4/IPv6 addresses simultaneously
- 🔒 **Secure** - Bearer token authentication for sensitive operations
- 📊 **Health Checks** - Built-in health monitoring endpoint
- ⚙️ **Configurable** - External YAML configuration for all settings
- 📝 **Detailed Logging** - Track cache hits/misses and errors
- 🐧 **Alpine Linux Ready** - Includes OpenRC service files

---

## 🤔 Why Use This Proxy?

The CDISC Library API provides valuable metadata about clinical trial data standards, but:

- **Rate Limiting** - Direct API calls may be rate-limited
- **Performance** - Reduce latency by caching frequently accessed resources
- **Reliability** - Continue serving cached data even if the upstream API is temporarily unavailable
- **Cost Efficiency** - Minimize API calls to stay within usage limits
- **Offline Development** - Work with cached CDISC metadata without constant internet connectivity

---

## 📦 Prerequisites

- **Go** 1.21 or later
- **Valkey** or **Redis** server
- **CDISC Library API Key** - [Get one here](https://www.cdisc.org/cdisc-library)

---

## 🚀 Quick Start

### 1. Clone the Repository

\`\`\`bash
git clone https://github.com/tomhub/cdisc-proxy.git
cd cdisc-proxy
\`\`\`

### 2. Install Dependencies

\`\`\`bash
go mod init cdisc-proxy
go get github.com/valkey-io/valkey-go
go get gopkg.in/yaml.v3
\`\`\`

### 3. Build

\`\`\`bash
go build -o cdisc-proxy main.go
\`\`\`

### 4. Configure

\`\`\`bash
# Create config directory
sudo mkdir -p /etc/conf.d

# Copy and edit configuration
cp config.yaml /etc/conf.d/cdisc-proxy.conf
nano /etc/conf.d/cdisc-proxy.conf
\`\`\`

**Required Configuration:**
- Add your CDISC API key
- Generate an auth key: \`openssl rand -hex 32\`
- Configure Valkey/Redis connection

### 5. Run

\`\`\`bash
./cdisc-proxy
\`\`\`

Or specify a custom config:

\`\`\`bash
./cdisc-proxy /path/to/config.yaml
\`\`\`

---

## ⚙️ Configuration

The proxy uses a YAML configuration file located at \`/etc/conf.d/cdisc-proxy.conf\` by default.

### Example Configuration

\`\`\`yaml
server:
  port: 8080
  listen:
    - "0.0.0.0"      # All IPv4 interfaces
    - "::"           # All IPv6 interfaces
  auth_key: "your-secure-random-key-here"

cdisc:
  base_url: "https://library.cdisc.org/api"
  api_key: "your-cdisc-api-key"

valkey:
  address: "localhost:6379"
  password: ""
  db: 0

cache:
  ttl: "1y"  # Supports: 24h, 7d, 30d, 1y, etc.
\`\`\`

### Configuration Options

| Section | Option | Description | Default |
|---------|--------|-------------|---------|
| \`server.port\` | Integer | Port to listen on | \`8080\` |
| \`server.listen\` | Array | IP addresses to bind (IPv4/IPv6) | \`["0.0.0.0"]\` |
| \`server.auth_key\` | String | Bearer token for \`/refresh\` endpoint | Required |
| \`cdisc.base_url\` | String | CDISC Library API base URL | Required |
| \`cdisc.api_key\` | String | Your CDISC API key | Required |
| \`valkey.address\` | String | Valkey/Redis server address | \`localhost:6379\` |
| \`valkey.password\` | String | Authentication password | Empty |
| \`valkey.db\` | Integer | Database number | \`0\` |
| \`cache.ttl\` | String | Cache duration (1y, 30d, 24h, etc.) | \`1y\` |

---

## 🔌 API Usage

### Regular Proxy Requests

Route requests through \`/api/*\` to access the CDISC Library API with automatic caching.

\`\`\`bash
# First request - fetches from CDISC and caches (X-Cache: MISS)
curl http://localhost:8080/api/mdr/sdtm/products/sdtmig/3-4

# Subsequent requests - served from cache (X-Cache: HIT)
curl http://localhost:8080/api/mdr/sdtm/products/sdtmig/3-4
\`\`\`

### Force Refresh

Update cached data using the authenticated \`/refresh\` endpoint:

\`\`\`bash
curl -X POST http://localhost:8080/refresh \\
  -H "Authorization: Bearer your-auth-key" \\
  -H "Content-Type: application/json" \\
  -d '{
    "path": "/mdr/sdtm/products/sdtmig/3-4"
  }'
\`\`\`

**Response:**
\`\`\`json
{
  "status": "success",
  "cached": true,
  "status_code": 200,
  "content_size": 12345
}
\`\`\`

### Health Check

Monitor service health:

\`\`\`bash
curl http://localhost:8080/health
\`\`\`

**Response:**
\`\`\`json
{
  "status": "healthy"
}
\`\`\`

---

## 💾 Cache Management

### Cache Keys

Cached responses are stored with the format: \`cdisc:cache:<path>\`

### Manual Cache Operations

\`\`\`bash
# Connect to Valkey/Redis
redis-cli

# List all cached keys
KEYS cdisc:cache:*

# View cached response
GET "cdisc:cache:/mdr/sdtm/products/sdtmig/3-4"

# Delete specific cache entry
DEL "cdisc:cache:/mdr/sdtm/products/sdtmig/3-4"

# Clear all cached data
KEYS cdisc:cache:* | xargs redis-cli DEL
\`\`\`

### TTL Management

Configure cache duration in \`config.yaml\`:

\`\`\`yaml
cache:
  ttl: "30d"  # Cache for 30 days
\`\`\`

Supported formats:
- **Years**: \`1y\`, \`2y\`
- **Weeks**: \`1w\`, \`4w\`
- **Days**: \`7d\`, \`30d\`, \`365d\`
- **Hours**: \`24h\`, \`168h\`
- **Go duration**: Any valid Go duration string

---

## 🚀 Deployment

### Alpine Linux (OpenRC)

#### Installation

\`\`\`bash
# Create user
adduser -D -s /sbin/nologin -h /var/lib/cdisc-proxy cdisc

# Install binary
install -m 755 cdisc-proxy /usr/local/bin/

# Configure
install -m 640 -o root -g cdisc config.yaml /etc/conf.d/cdisc-proxy.conf

# Install service
install -m 755 cdisc-proxy.initd /etc/init.d/cdisc-proxy

# Create log directory
mkdir -p /var/log/cdisc-proxy
chown cdisc:cdisc /var/log/cdisc-proxy
\`\`\`

#### Service Management

\`\`\`bash
# Enable on boot
rc-update add cdisc-proxy default

# Start service
rc-service cdisc-proxy start

# View logs
tail -f /var/log/cdisc-proxy/output.log
\`\`\`

### Systemd

#### Service File

Create \`/etc/systemd/system/cdisc-proxy.service\`:

\`\`\`ini
[Unit]
Description=CDISC Library API Proxy
After=network.target valkey.service

[Service]
Type=simple
User=cdisc
Group=cdisc
ExecStart=/usr/local/bin/cdisc-proxy
Restart=on-failure
RestartSec=5s

StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
\`\`\`

#### Enable and Start

\`\`\`bash
sudo systemctl enable cdisc-proxy
sudo systemctl start cdisc-proxy
sudo systemctl status cdisc-proxy
\`\`\`

### Docker

#### Dockerfile

\`\`\`dockerfile
FROM golang:1.21-alpine AS builder

WORKDIR /build
COPY . .

RUN go mod download && \\
    go build -o cdisc-proxy main.go

FROM alpine:latest

RUN apk --no-cache add ca-certificates && \\
    adduser -D -s /sbin/nologin cdisc

WORKDIR /app
COPY --from=builder /build/cdisc-proxy .

USER cdisc
EXPOSE 8080

CMD ["./cdisc-proxy"]
\`\`\`

#### Build and Run

\`\`\`bash
# Build image
docker build -t cdisc-proxy .

# Run container
docker run -d \\
  --name cdisc-proxy \\
  -p 8080:8080 \\
  -v $(pwd)/config.yaml:/etc/conf.d/cdisc-proxy.conf:ro \\
  cdisc-proxy
\`\`\`

#### Docker Compose

\`\`\`yaml
version: '3.8'

services:
  cdisc-proxy:
    build: .
    ports:
      - "8080:8080"
    volumes:
      - ./config.yaml:/etc/conf.d/cdisc-proxy.conf:ro
    depends_on:
      - valkey
    restart: unless-stopped

  valkey:
    image: valkey/valkey:latest
    volumes:
      - valkey-data:/data
    restart: unless-stopped

volumes:
  valkey-data:
\`\`\`

---

## 📊 Monitoring

### Logs

The application logs:
- Configuration loading
- Cache TTL settings
- Listen addresses
- Cache hits and misses
- Upstream API errors
- Refresh requests

**Example Output:**
\`\`\`
2025/11/16 10:00:00 Loading configuration from: /etc/conf.d/cdisc-proxy.conf
2025/11/16 10:00:00 Cache TTL set to: 8760h0m0s
2025/11/16 10:00:00 Starting CDISC Library API Proxy on port 8080
2025/11/16 10:00:00 Listening on: [0.0.0.0 ::]
2025/11/16 10:00:00 Starting listener on 0.0.0.0:8080
2025/11/16 10:00:00 Starting listener on [::]:8080
2025/11/16 10:00:05 Cache miss for: /mdr/sdtm/products/sdtmig/3-4
2025/11/16 10:00:06 Cache hit for: /mdr/sdtm/products/sdtmig/3-4
\`\`\`

### Metrics

Monitor these key metrics:
- **Cache hit rate** - Higher is better
- **Response time** - Should be <50ms for cache hits
- **Valkey connection status** - Check \`/health\` endpoint
- **Upstream API errors** - Watch error logs

---

## 🔒 Security

### Best Practices

1. **Protect Auth Key**
   \`\`\`bash
   # Generate strong key
   openssl rand -hex 32
   
   # Secure config file
   chmod 640 /etc/conf.d/cdisc-proxy.conf
   chown root:cdisc /etc/conf.d/cdisc-proxy.conf
   \`\`\`

2. **Use HTTPS in Production**
   - Run behind nginx or Caddy with SSL/TLS
   - Never expose directly to the internet

3. **Firewall Configuration**
   \`\`\`bash
   # Allow only specific IPs
   iptables -A INPUT -p tcp --dport 8080 -s 192.168.1.0/24 -j ACCEPT
   iptables -A INPUT -p tcp --dport 8080 -j DROP
   \`\`\`

4. **Network Isolation**
   \`\`\`yaml
   server:
     listen:
       - "127.0.0.1"  # Localhost only
       - "::1"
   \`\`\`

5. **Regular Updates**
   - Keep Go and dependencies up to date
   - Monitor security advisories
   - Rotate API keys periodically

---

## 🔧 Troubleshooting

### Service Won't Start

**Check configuration:**
\`\`\`bash
./cdisc-proxy /etc/conf.d/cdisc-proxy.conf
\`\`\`

**Verify permissions:**
\`\`\`bash
ls -la /etc/conf.d/cdisc-proxy.conf
# Should be readable by cdisc user
\`\`\`

### Valkey Connection Failed

**Test connection:**
\`\`\`bash
redis-cli -h localhost -p 6379 ping
\`\`\`

**Check if Valkey is running:**
\`\`\`bash
# Alpine/OpenRC
rc-service valkey status

# Systemd
systemctl status valkey
\`\`\`

### Cache Not Working

**Verify cache operations:**
\`\`\`bash
# Check if keys are being created
redis-cli KEYS "cdisc:cache:*"

# Monitor cache activity
redis-cli MONITOR
\`\`\`

**Check TTL:**
\`\`\`bash
redis-cli TTL "cdisc:cache:/your/path"
\`\`\`

### Port Already in Use

**Find process using port:**
\`\`\`bash
lsof -i :8080
# or
netstat -tulpn | grep 8080
\`\`\`

**Use different port:**
\`\`\`yaml
server:
  port: 8081
\`\`\`

### Unauthorized Errors

**Verify auth key:**
\`\`\`bash
# Check config
grep auth_key /etc/conf.d/cdisc-proxy.conf

# Test with correct key
curl -X POST http://localhost:8080/refresh \\
  -H "Authorization: Bearer $(grep auth_key /etc/conf.d/cdisc-proxy.conf | cut -d'"' -f2)" \\
  -H "Content-Type: application/json" \\
  -d '{"path": "/test"}'
\`\`\`

---

## 🤝 Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

### Development Setup

\`\`\`bash
# Clone repository
git clone https://github.com/tomhub/cdisc-proxy.git
cd cdisc-proxy

# Install dependencies
go mod download

# Run tests (if available)
go test ./...

# Build
go build -o cdisc-proxy main.go

# Run locally
./cdisc-proxy config.yaml
\`\`\`

### Guidelines

- Follow Go best practices and idioms
- Add tests for new features
- Update documentation
- Keep commits atomic and well-described

---

## 📄 License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

---

## 🙏 Acknowledgments

- [CDISC](https://www.cdisc.org/) - For providing the clinical data standards and API
- [Valkey](https://valkey.io/) - High-performance key-value store
- Go Community - For excellent tools and libraries

---

## 📞 Support

- **Issues**: [GitHub Issues](https://github.com/tomhub/cdisc-proxy/issues)
- **Discussions**: [GitHub Discussions](https://github.com/tomhub/cdisc-proxy/discussions)
- **CDISC Library**: [CDISC Documentation](https://www.cdisc.org/cdisc-library)

---

<div align="center">
Made with ❤️ for the clinical research community
</div>
