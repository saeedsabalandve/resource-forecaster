import http from 'k6/http';
import { check, sleep, group } from 'k6';
import { Rate, Trend, Counter } from 'k6/metrics';
import { textSummary } from 'https://jslib.k6.io/k6-summary/0.0.1/index.js';

// # Custom metrics for detailed performance analysis
const forecastErrors = new Rate('forecast_errors');
const forecastDuration = new Trend('forecast_duration', true);
const metricsCollectionErrors = new Rate('metrics_collection_errors');
const concurrentUsers = new Counter('concurrent_users');

// # Production-grade load test configuration
export const options = {
    // # Multiple scenarios simulating different workload patterns
    scenarios: {
        // # Steady baseline load for forecast generation
        forecast_baseline: {
            executor: 'ramping-vus',
            startVUs: 10,
            stages: [
                { duration: '5m', target: 50 },   // # Ramp up to normal load
                { duration: '20m', target: 50 },   // # Stay at normal load
                { duration: '5m', target: 100 },   // # Ramp up to peak load
                { duration: '10m', target: 100 },  // # Stay at peak load
                { duration: '5m', target: 10 },    // # Scale down
            ],
            exec: 'forecastScenario',
            tags: { scenario: 'forecast' },
        },
        
        // # Spike testing for metrics collection
        metrics_spike: {
            executor: 'ramping-arrival-rate',
            startRate: 50,
            timeUnit: '1s',
            preAllocatedVUs: 20,
            maxVUs: 200,
            stages: [
                { duration: '2m', target: 50 },    // # Normal rate
                { duration: '1m', target: 500 },   // # Spike to 10x
                { duration: '2m', target: 50 },    // # Recovery
            ],
            exec: 'metricsScenario',
            tags: { scenario: 'metrics' },
        },
        
        // # Stress test for health endpoints
        health_check: {
            executor: 'constant-arrival-rate',
            rate: 100,
            timeUnit: '1s',
            duration: '30m',
            preAllocatedVUs: 10,
            exec: 'healthScenario',
            tags: { scenario: 'health' },
        },
    },
    
    // # Thresholds for SLO compliance
    thresholds: {
        http_req_duration: [
            'p(95)<500',     // # 95th percentile must be under 500ms
            'p(99)<1000',    // # 99th percentile must be under 1s
            'max<2000',      // # Maximum allowed response time
        ],
        http_req_failed: [
            'rate<0.01',     // # Less than 1% error rate
        ],
        'forecast_duration': [
            'p(95)<3000',    // # Forecast generation under 3s
        ],
        'forecast_errors': [
            'rate<0.05',     // # Less than 5% forecast errors
        ],
        'metrics_collection_errors': [
            'rate<0.02',     // # Less than 2% collection errors
        ],
    },
};

// # Test data generation for realistic scenarios
const testData = {
    forecastPeriods: ['1h', '6h', '12h', '24h', '168h'],  // # Different forecast horizons
    metricTypes: ['cpu', 'memory', 'disk', 'network'],
    resourceTypes: ['hourly', 'daily', 'weekly'],
};

// # Setup function to prepare test environment
export function setup() {
    console.log('Starting load test setup...');
    
    // # Warm up the forecasting models
    const warmupPayload = JSON.stringify({
        hostname: 'test-instance-01',
        metricType: 'cpu',
        periodHours: 168,
    });
    
    const warmupResponse = http.post(
        'http://resource-forecaster.monitoring.svc.cluster.local:8080/api/v1/forecast',
        warmupPayload,
        {
            headers: {
                'Content-Type': 'application/json',
                'Authorization': `Bearer ${__ENV.API_TOKEN}`,
            },
            timeout: '30s',
        }
    );
    
    check(warmupResponse, {
        'warmup forecast successful': (r) => r.status === 200,
    });
    
    return {
        apiBaseUrl: 'http://resource-forecaster.monitoring.svc.cluster.local:8080',
        authToken: __ENV.API_TOKEN,
    };
}

// # Main forecast scenario simulating production workload
export function forecastScenario(data) {
    const periodIndex = Math.floor(Math.random() * testData.forecastPeriods.length);
    const period = testData.forecastPeriods[periodIndex];
    
    group('Generate Forecast', () => {
        const payload = JSON.stringify({
            hostname: `test-instance-${Math.floor(Math.random() * 50)}`,
            metricType: testData.metricTypes[Math.floor(Math.random() * testData.metricTypes.length)],
            periodHours: parseInt(period),
            confidenceInterval: 0.95,
            includeBreakdown: true,
        });
        
        const startTime = Date.now();
        
        const response = http.post(
            `${data.apiBaseUrl}/api/v1/forecast`,
            payload,
            {
                headers: {
                    'Content-Type': 'application/json',
                    'Authorization': `Bearer ${data.authToken}`,
                    'X-Request-ID': `${__VU}-${__ITER}`,
                },
                tags: { name: 'forecast' },
                timeout: '10s',
            }
        );
        
        forecastDuration.add(Date.now() - startTime);
        
        const success = check(response, {
            'forecast status 200': (r) => r.status === 200,
            'forecast response has predictions': (r) => {
                try {
                    const body = JSON.parse(r.body);
                    return body.predictions && body.predictions.length > 0;
                } catch (e) {
                    return false;
                }
            },
            'forecast has confidence intervals': (r) => {
                try {
                    const body = JSON.parse(r.body);
                    return body.lowerBound && body.upperBound && body.confidenceScore;
                } catch (e) {
                    return false;
                }
            },
        });
        
        if (!success) {
            forecastErrors.add(1);
            console.error(`Forecast failed for ${period}: ${response.status} - ${response.body}`);
        }
        
        // # Simulate realistic user behavior
        sleep(Math.random() * 5 + 1);
    });
}

// # Metrics submission scenario
export function metricsScenario(data) {
    group('Submit Metrics', () => {
        const metricsPayload = JSON.stringify({
            hostname: `node-${__VU}-${__ITER}`,
            instanceId: `i-${Math.random().toString(36).substring(2, 10)}`,
            timestamp: new Date().toISOString(),
            cpu: {
                usagePercent: Math.random() * 100,
                userPercent: Math.random() * 80,
                systemPercent: Math.random() * 20,
                iowaitPercent: Math.random() * 10,
                numCores: 4,
            },
            memory: {
                totalBytes: 17179869184,
                usedBytes: Math.random() * 17179869184,
                usedPercent: Math.random() * 100,
            },
            disk: [
                {
                    mountPoint: '/',
                    usedPercent: Math.random() * 95,
                    totalBytes: 107374182400,
                    usedBytes: Math.random() * 107374182400,
                }
            ],
            network: {
                receivedBytesPerSec: Math.random() * 125000000,
                transmittedBytesPerSec: Math.random() * 125000000,
            },
        });
        
        const response = http.post(
            `${data.apiBaseUrl}/api/v1/metrics`,
            metricsPayload,
            {
                headers: {
                    'Content-Type': 'application/json',
                    'Authorization': `Bearer ${data.authToken}`,
                },
                tags: { name: 'metrics' },
            }
        );
        
        const success = check(response, {
            'metrics submission successful': (r) => r.status === 201 || r.status === 200,
        });
        
        if (!success) {
            metricsCollectionErrors.add(1);
        }
        
        sleep(1);
    });
}

// # Health check scenario
export function healthScenario(data) {
    const checks = {
        'health_live': '/health/live',
        'health_ready': '/health/ready',
    };
    
    for (const [name, endpoint] of Object.entries(checks)) {
        const response = http.get(
            `${data.apiBaseUrl}${endpoint}`,
            {
                tags: { name },
                timeout: '5s',
            }
        );
        
        check(response, {
            [`${name} status 200`]: (r) => r.status === 200,
            [`${name} response time < 100ms`]: (r) => r.timings.duration < 100,
        });
    }
    
    sleep(1);
}

// # Teardown function for cleanup
export function teardown(data) {
    console.log('Load test completed, generating summary...');
}

// # Custom summary with detailed performance analysis
export function handleSummary(data) {
    const summary = {
        timestamp: new Date().toISOString(),
        test_duration_seconds: data.state.testRunDurationMs / 1000,
        
        // # Overall statistics
        total_requests: data.metrics.http_reqs?.values?.count || 0,
        total_failures: data.metrics.http_req_failed?.values?.passes || 0,
        failure_rate: data.metrics.http_req_failed?.values?.rate || 0,
        
        // # Performance percentiles
        response_times: {
            avg: data.metrics.http_req_duration?.values?.avg || 0,
            p50: data.metrics.http_req_duration?.values?.med || 0,
            p90: data.metrics.http_req_duration?.values?.['p(90)'] || 0,
            p95: data.metrics.http_req_duration?.values?.['p(95)'] || 0,
            p99: data.metrics.http_req_duration?.values?.['p(99)'] || 0,
            max: data.metrics.http_req_duration?.values?.max || 0,
        },
        
        // # Custom metrics
        forecast: {
            errors_rate: data.metrics.forecast_errors?.values?.rate || 0,
            avg_duration: data.metrics.forecast_duration?.values?.avg || 0,
            p95_duration: data.metrics.forecast_duration?.values?.['p(95)'] || 0,
        },
        
        metrics_collection: {
            errors_rate: data.metrics.metrics_collection_errors?.values?.rate || 0,
        },
        
        // # SLO compliance
        slo_compliance: {
            response_time_p95_under_500ms: (data.metrics.http_req_duration?.values?.['p(95)'] || 0) < 500,
            error_rate_under_1pct: (data.metrics.http_req_failed?.values?.rate || 0) < 0.01,
            forecast_p95_under_3s: (data.metrics.forecast_duration?.values?.['p(95)'] || 0) < 3000,
        },
        
        // # Recommendations based on results
        recommendations: [],
    };
    
    // # Generate recommendations based on results
    if (summary.slo_compliance.response_time_p95_under_500ms) {
        summary.recommendations.push('Response time meets SLO');
    } else {
        summary.recommendations.push('URGENT: Response time exceeds SLO - investigate bottlenecks');
    }
    
    return {
        'stdout': textSummary(data, { indent: '  ', enableColors: true }),
        'summary.json': JSON.stringify(summary, null, 2),
    };
      }
