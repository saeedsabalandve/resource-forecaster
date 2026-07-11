#!/usr/bin/env bash

# # Database seeding script for development and testing
# # Generates realistic historical metrics data

set -euo pipefail

# # Configuration
DB_HOST="${TSDB_HOST:-localhost}"
DB_PORT="${TSDB_PORT:-5432}"
DB_NAME="${TSDB_DATABASE:-resource_forecaster}"
DB_USER="${TSDB_USERNAME:-forecaster}"
DB_PASSWORD="${TSDB_PASSWORD:-forecaster}"

# # Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# # Helper functions
log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# # Check if psql is available
if ! command -v psql &> /dev/null; then
    log_error "psql could not be found. Please install PostgreSQL client."
    exit 1
fi

# # Export password for psql
export PGPASSWORD="$DB_PASSWORD"

# # Connection string
CONN_STRING="host=$DB_HOST port=$DB_PORT dbname=$DB_NAME user=$DB_USER sslmode=disable"

log_info "Starting data seeding for Resource Forecaster..."

# # Test connection
if ! psql "$CONN_STRING" -c "SELECT 1;" > /dev/null 2>&1; then
    log_error "Failed to connect to TimescaleDB at $DB_HOST:$DB_PORT"
    exit 1
fi

log_info "Connected to TimescaleDB successfully"

# # Create TimescaleDB extension if not exists
psql "$CONN_STRING" -c "CREATE EXTENSION IF NOT EXISTS timescaledb CASCADE;" > /dev/null 2>&1

# # Function to generate CPU metrics
generate_cpu_data() {
    local hostname=$1
    local start_time=$2
    local hours=$3
    
    log_info "Generating CPU metrics for $hostname ($hours hours)..."
    
    # # Generate INSERT statements using a temporary SQL script
    local tmpfile=$(mktemp)
    
    cat > "$tmpfile" << 'EOF'
DO $$
DECLARE
    v_hostname TEXT := '<HOSTNAME>';
    v_start TIMESTAMPTZ := '<START_TIME>';
    v_hours INT := <HOURS>;
    v_ts TIMESTAMPTZ;
    v_cpu FLOAT;
    v_base_cpu FLOAT;
    v_time_of_day FLOAT;
    v_day_of_week FLOAT;
    v_noise FLOAT;
BEGIN
    -- # Different base CPU for different servers
    v_base_cpu := CASE 
        WHEN v_hostname LIKE '%prod%' THEN 65.0
        WHEN v_hostname LIKE '%staging%' THEN 35.0
        ELSE 20.0
    END;
    
    FOR i IN 0..(v_hours * 12) LOOP  -- # 5-minute intervals
        v_ts := v_start + (i * INTERVAL '5 minutes');
        
        -- # Time-of-day pattern (higher during business hours)
        v_time_of_day := CASE 
            WHEN EXTRACT(HOUR FROM v_ts) BETWEEN 9 AND 17 THEN 15.0
            WHEN EXTRACT(HOUR FROM v_ts) BETWEEN 6 AND 9 THEN 5.0
            WHEN EXTRACT(HOUR FROM v_ts) BETWEEN 17 AND 22 THEN 10.0
            ELSE -10.0
        END;
        
        -- # Day-of-week pattern (lower on weekends)
        v_day_of_week := CASE
            WHEN EXTRACT(DOW FROM v_ts) IN (0, 6) THEN -20.0
            ELSE 0.0
        END;
        
        -- # Random noise
        v_noise := (random() * 20) - 10;
        
        -- # Calculate final CPU value with constraints
        v_cpu := GREATEST(0.0, LEAST(100.0, v_base_cpu + v_time_of_day + v_day_of_week + v_noise));
        
        -- # Insert CPU metrics
        INSERT INTO cpu_metrics (
            timestamp, hostname, instance_id, instance_type,
            usage_percent, user_percent, system_percent, 
            iowait_percent, steal_percent, num_cores, throttled_time
        ) VALUES (
            v_ts, v_hostname, 
            'i-' || substring(md5(v_hostname || v_ts::text) from 1 for 8),
            CASE 
                WHEN v_base_cpu > 50 THEN 'm5.xlarge'
                WHEN v_base_cpu > 30 THEN 'm5.large'
                ELSE 't3.medium'
            END,
            v_cpu,
            v_cpu * 0.7,    -- # User CPU
            v_cpu * 0.2,    -- # System CPU
            v_cpu * 0.05,   -- # IO Wait
            v_cpu * 0.05,   -- # Steal
            CASE WHEN v_base_cpu > 50 THEN 4 ELSE 2 END,
            CASE WHEN v_cpu > 90 THEN random() * 10 ELSE 0 END
        );
        
        -- # Insert Memory metrics
        INSERT INTO memory_metrics (
            timestamp, hostname, instance_id,
            total_bytes, used_bytes, available_bytes, used_percent,
            swap_total_bytes, swap_used_bytes, cached_bytes, buffers_bytes
        ) VALUES (
            v_ts, v_hostname, 
            'i-' || substring(md5(v_hostname || v_ts::text) from 1 for 8),
            17179869184,  -- # 16 GB
            (17179869184 * (v_cpu * 0.8 + random() * 20) / 100)::BIGINT,
            17179869184 - (17179869184 * (v_cpu * 0.8 + random() * 20) / 100)::BIGINT,
            v_cpu * 0.8 + random() * 20,
            2147483648,   -- # 2 GB swap
            (2147483648 * random() * 0.3)::BIGINT,
            (17179869184 * 0.2)::BIGINT,
            (17179869184 * 0.1)::BIGINT
        );
        
        -- # Insert Load Average metrics
        INSERT INTO load_average_metrics (
            timestamp, hostname, instance_id,
            load1, load5, load15
        ) VALUES (
            v_ts, v_hostname,
            'i-' || substring(md5(v_hostname || v_ts::text) from 1 for 8),
            v_cpu / 25.0 + random() * 0.5,
            v_cpu / 25.0 + random() * 0.3,
            v_cpu / 25.0 + random() * 0.2
        );
        
        -- # Commit every 1000 rows to avoid large transactions
        IF i % 1000 = 0 THEN
            COMMIT;
        END IF;
        
    END LOOP;
END $$;
EOF
    
    # # Replace placeholders
    sed -i "s|<HOSTNAME>|$hostname|g" "$tmpfile"
    sed -i "s|<START_TIME>|$start_time|g" "$tmpfile"
    sed -i "s|<HOURS>|$hours|g" "$tmpfile"
    
    # # Execute the SQL script
    psql "$CONN_STRING" -f "$tmpfile" -q
    
    rm "$tmpfile"
    
    log_info "CPU metrics generated for $hostname"
}

# # Generate data for multiple hosts
HOSTS=(
    "prod-web-server-01"
    "prod-web-server-02"
    "prod-db-server-01"
    "staging-app-server-01"
    "dev-server-01"
)

START_DATE="2023-06-01 00:00:00 UTC"

for host in "${HOSTS[@]}"; do
    # # Generate 90 days of data
    generate_cpu_data "$host" "$START_DATE" 2160
    
    # # Progress indicator
    echo -n "."
done

echo "" # New line after progress dots

# # Create indexes for better query performance
log_info "Creating performance indexes..."

psql "$CONN_STRING" << 'EOF'
-- # Create composite indexes for common queries
CREATE INDEX IF NOT EXISTS idx_cpu_hostname_timestamp 
    ON cpu_metrics (hostname, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_memory_hostname_timestamp 
    ON memory_metrics (hostname, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_load_hostname_timestamp 
    ON load_average_metrics (hostname, timestamp DESC);

-- # Update table statistics
ANALYZE cpu_metrics;
ANALYZE memory_metrics;
ANALYZE load_average_metrics;
EOF

log_info "Indexes created"

# # Create continuous aggregates for faster queries
log_info "Creating materialized views..."

psql "$CONN_STRING" << 'EOF'
-- # Refresh continuous aggregates
REFRESH MATERIALIZED VIEW cpu_metrics_hourly;
REFRESH MATERIALIZED VIEW cpu_metrics_daily;
EOF

log_info "Materialized views refreshed"

# # Verify data was inserted
log_info "Verifying data..."

ROW_COUNTS=$(psql "$CONN_STRING" -t << 'EOF'
SELECT 
    'CPU Metrics: ' || COUNT(*) || ' rows' FROM cpu_metrics
UNION ALL
SELECT 
    'Memory Metrics: ' || COUNT(*) || ' rows' FROM memory_metrics
UNION ALL
SELECT 
    'Load Average: ' || COUNT(*) || ' rows' FROM load_average_metrics;
EOF
)

echo "$ROW_COUNTS"

log_info "Database seeding completed successfully! 🎉"
log_info "You can now start the forecaster service and begin generating predictions."
