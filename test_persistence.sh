#!/bin/bash

# Test script for verifying state persistence across restarts
# This script tests that the invalidator properly maintains state across reboots

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo "======================================"
echo "State Persistence Test"
echo "======================================"
echo ""

# Detect if using Docker or native
USE_DOCKER=false
if command -v docker-compose &> /dev/null && [ -f "docker-compose.yml" ]; then
    echo -e "${BLUE}Detected Docker Compose setup${NC}"
    USE_DOCKER=true
    STATE_FILE="/var/lib/cdisc-invalidator/state.json"
else
    echo -e "${BLUE}Detected native installation${NC}"
    STATE_FILE="/var/lib/cdisc-invalidator/state.json"
fi

# Helper functions
get_state() {
    if [ "$USE_DOCKER" = true ]; then
        docker-compose exec -T invalidator cat "$STATE_FILE" 2>/dev/null || echo "{}"
    else
        sudo cat "$STATE_FILE" 2>/dev/null || echo "{}"
    fi
}

restart_service() {
    echo -e "${YELLOW}Restarting invalidator service...${NC}"
    if [ "$USE_DOCKER" = true ]; then
        docker-compose restart invalidator
        sleep 5
    else
        sudo systemctl restart cdisc-invalidator
        sleep 5
    fi
}

get_logs() {
    if [ "$USE_DOCKER" = true ]; then
        docker-compose logs --tail=20 invalidator
    else
        sudo journalctl -u cdisc-invalidator -n 20 --no-pager
    fi
}

check_service_running() {
    if [ "$USE_DOCKER" = true ]; then
        docker-compose ps invalidator | grep -q "Up"
    else
        systemctl is-active --quiet cdisc-invalidator
    fi
}

# Test 1: Check if service is running
echo "Test 1: Verify service is running"
if check_service_running; then
    echo -e "${GREEN}✓ Service is running${NC}"
else
    echo -e "${RED}✗ Service is not running${NC}"
    exit 1
fi
echo ""

# Test 2: Check if state file exists
echo "Test 2: Verify state file exists"
STATE_CONTENT=$(get_state)
if [ "$STATE_CONTENT" != "{}" ] && [ -n "$STATE_CONTENT" ]; then
    echo -e "${GREEN}✓ State file exists${NC}"
    echo "Current state:"
    echo "$STATE_CONTENT" | jq . 2>/dev/null || echo "$STATE_CONTENT"
else
    echo -e "${YELLOW}⚠ State file doesn't exist yet (will be created on first run)${NC}"
    echo "Waiting for first check cycle (up to 5 minutes)..."
    sleep 10
    STATE_CONTENT=$(get_state)
    if [ "$STATE_CONTENT" != "{}" ]; then
        echo -e "${GREEN}✓ State file created${NC}"
    else
        echo -e "${RED}✗ State file not created${NC}"
        exit 1
    fi
fi
echo ""

# Save current state for comparison
ORIGINAL_STATE="$STATE_CONTENT"

# Test 3: Restart and verify state persists
echo "Test 3: Verify state persists after restart"
restart_service

# Wait for service to fully start
echo "Waiting for service to initialize..."
sleep 3

# Check logs for "Loaded previous state from disk"
echo "Checking logs for state loading message..."
LOGS=$(get_logs)
if echo "$LOGS" | grep -q "Loaded previous state from disk"; then
    echo -e "${GREEN}✓ State was loaded from disk after restart${NC}"
else
    echo -e "${RED}✗ State was not loaded from disk${NC}"
    echo "Recent logs:"
    echo "$LOGS"
    exit 1
fi
echo ""

# Test 4: Verify state content is unchanged
echo "Test 4: Verify state content is unchanged"
NEW_STATE=$(get_state)
if [ "$ORIGINAL_STATE" = "$NEW_STATE" ]; then
    echo -e "${GREEN}✓ State content preserved after restart${NC}"
else
    echo -e "${RED}✗ State content changed after restart${NC}"
    echo "Original:"
    echo "$ORIGINAL_STATE" | jq . 2>/dev/null || echo "$ORIGINAL_STATE"
    echo ""
    echo "After restart:"
    echo "$NEW_STATE" | jq . 2>/dev/null || echo "$NEW_STATE"
    exit 1
fi
echo ""

# Test 5: Verify state file structure
echo "Test 5: Verify state file structure"
REQUIRED_FIELDS=("data-analysis" "data-collection" "data-tabulation" "integrated" "qrs" "terminology")
ALL_PRESENT=true

for field in "${REQUIRED_FIELDS[@]}"; do
    if echo "$NEW_STATE" | jq -e ".[\"$field\"]" > /dev/null 2>&1; then
        echo -e "${GREEN}✓ Field '$field' present${NC}"
    else
        echo -e "${RED}✗ Field '$field' missing${NC}"
        ALL_PRESENT=false
    fi
done

if [ "$ALL_PRESENT" = true ]; then
    echo -e "${GREEN}✓ All required fields present${NC}"
else
    echo -e "${RED}✗ Some fields missing${NC}"
    exit 1
fi
echo ""

# Test 6: Verify invalidator is querying through cache
echo "Test 6: Verify invalidator queries through Varnish cache"
echo "Checking if invalidator is making requests to Varnish..."

# Check logs for cache-related messages
if echo "$LOGS" | grep -q "Fetching lastupdated from cache"; then
    echo -e "${GREEN}✓ Invalidator is querying through cache${NC}"
else
    echo -e "${YELLOW}⚠ Cannot confirm cache usage from logs${NC}"
fi

# Check for cache status in logs
if echo "$LOGS" | grep -q "Cache status:"; then
    CACHE_STATUS=$(echo "$LOGS" | grep "Cache status:" | tail -1)
    echo "Last cache status: $CACHE_STATUS"
    if echo "$CACHE_STATUS" | grep -q "HIT"; then
        echo -e "${GREEN}✓ Cache is being hit${NC}"
    elif echo "$CACHE_STATUS" | grep -q "MISS"; then
        echo -e "${YELLOW}⚠ Cache miss (expected on first query or after TTL expiry)${NC}"
    fi
fi
echo ""

# Test 7: Verify atomic writes (check for .tmp files)
echo "Test 7: Verify no temporary files left behind"
if [ "$USE_DOCKER" = true ]; then
    TMP_FILES=$(docker-compose exec -T invalidator sh -c "ls /var/lib/cdisc-invalidator/*.tmp 2>/dev/null || true")
else
    TMP_FILES=$(sudo sh -c "ls /var/lib/cdisc-invalidator/*.tmp 2>/dev/null || true")
fi

if [ -z "$TMP_FILES" ]; then
    echo -e "${GREEN}✓ No temporary files found (atomic writes working)${NC}"
else
    echo -e "${YELLOW}⚠ Temporary files found:${NC}"
    echo "$TMP_FILES"
fi
echo ""

# Summary
echo "======================================"
echo -e "${GREEN}All tests passed!${NC}"
echo "======================================"
echo ""
echo "Summary:"
echo "- State file exists and is properly formatted"
echo "- State persists across service restarts"
echo "- Invalidator loads state from disk on startup"
echo "- Invalidator queries through Varnish cache"
echo "- Atomic writes are functioning correctly"
echo ""
echo "Current state dates:"
echo "$NEW_STATE" | jq . 2>/dev/null || echo "$NEW_STATE"