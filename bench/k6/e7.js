// E7: does the circuit breaker protect latency and errors when one backend
// starts failing? Phases (after warm-up): before -> failing -> recovered.
// The script itself flips the victim backend's error rate at FAIL_AT and
// back at RECOVER_AT, so phase boundaries line up with the injected failure.
// Env: TARGET, VICTIM_ADMIN (backend base URL), RATE, WARMUP, FAIL_AT,
//      RECOVER_AT, END, ERROR_RATE, SUMMARY.
import http from 'k6/http';
import exec from 'k6/execution';

const target = __ENV.TARGET || 'http://127.0.0.1:18080';
const victim = __ENV.VICTIM_ADMIN || 'http://127.0.0.1:9101';
const rate = parseInt(__ENV.RATE || '500', 10);
const secs = (k, d) => parseInt(__ENV[k] || d, 10);
const warmup = secs('WARMUP', '10');
const failAt = secs('FAIL_AT', '30');
const recoverAt = secs('RECOVER_AT', '70');
const end = secs('END', '100');
const errorRate = __ENV.ERROR_RATE || '0.5';

function phase() {
  const t = exec.instance.currentTestRunDuration / 1000;
  if (t < warmup) return 'warmup';
  if (t < failAt) return 'before';
  if (t < recoverAt) return 'failing';
  return 'recovered';
}

const sub = {};
for (const p of ['before', 'failing', 'recovered']) {
  sub[`http_req_duration{phase:${p}}`] = ['max>=0'];
  sub[`http_req_failed{phase:${p}}`] = ['rate>=0'];
}

export const options = {
  discardResponseBodies: true,
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  scenarios: {
    load: {
      executor: 'constant-arrival-rate', rate, timeUnit: '1s', duration: `${end}s`,
      preAllocatedVUs: Math.max(50, Math.ceil(rate / 20)), maxVUs: 2000,
    },
    inject: { executor: 'shared-iterations', iterations: 1, vus: 1, startTime: `${failAt}s`, exec: 'inject' },
    recover: { executor: 'shared-iterations', iterations: 1, vus: 1, startTime: `${recoverAt}s`, exec: 'recover' },
  },
  thresholds: { ...sub, 'dropped_iterations{scenario:load}': ['count>=0'] },
};

export default function () {
  http.get(`${target}/e7`, { tags: { phase: phase() } });
}

export function inject() {
  http.post(`${victim}/admin/config?error_rate=${errorRate}`, null, { tags: { phase: 'admin' } });
}

export function recover() {
  http.post(`${victim}/admin/config?error_rate=0`, null, { tags: { phase: 'admin' } });
}

export function handleSummary(data) {
  return { [__ENV.SUMMARY || 'summary.json']: JSON.stringify(data) };
}
