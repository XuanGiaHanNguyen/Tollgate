// E1: gateway throughput and latency at a fixed arrival rate.
// Env: TARGETS (comma-separated base URLs, requests round-robin across them),
//      RATE (req/s), DURATION (measured window), WARMUP, MAX_VUS, SUMMARY (output path).
import http from 'k6/http';
import exec from 'k6/execution';

const targets = (__ENV.TARGETS || 'http://127.0.0.1:18080').split(',');
const rate = parseInt(__ENV.RATE || '1000', 10);
const duration = __ENV.DURATION || '60s';
const warmup = __ENV.WARMUP || '10s';
const maxVUs = parseInt(__ENV.MAX_VUS || '2000', 10);
const preVUs = Math.min(maxVUs, Math.max(50, Math.ceil(rate / 50)));

export const options = {
  discardResponseBodies: true,
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  scenarios: {
    warmup: {
      executor: 'constant-arrival-rate', rate, timeUnit: '1s', duration: warmup,
      preAllocatedVUs: preVUs, maxVUs,
    },
    measure: {
      executor: 'constant-arrival-rate', rate, timeUnit: '1s', duration, startTime: warmup,
      preAllocatedVUs: preVUs, maxVUs,
    },
  },
  // Thresholds that always pass, only so the summary includes these submetrics.
  thresholds: {
    'http_req_duration{scenario:measure}': ['max>=0'],
    'http_req_failed{scenario:measure}': ['rate>=0'],
    'http_reqs{scenario:measure}': ['count>=0'],
    'dropped_iterations{scenario:measure}': ['count>=0'],
  },
};

export default function () {
  const t = targets[exec.scenario.iterationInTest % targets.length];
  http.get(`${t}/e1`);
}

export function handleSummary(data) {
  return { [__ENV.SUMMARY || 'summary.json']: JSON.stringify(data) };
}
