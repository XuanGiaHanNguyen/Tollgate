// E3: accuracy of distributed rate limiting. One API key (one user), a
// fixed arrival rate spread round-robin over TARGETS. Counts admitted (200)
// and limited (429) responses in the measured window, optionally split into
// phases around KILL_AT (seconds from start) for the gateway-kill run.
// Env: TARGETS, RATE, WARMUP, DURATION (seconds), KILL_AT (0 = none), API_KEY, SUMMARY.
import http from 'k6/http';
import exec from 'k6/execution';

const targets = (__ENV.TARGETS || 'http://127.0.0.1:18081').split(',');
const rate = parseInt(__ENV.RATE || '300', 10);
const warmup = parseInt(__ENV.WARMUP || '10', 10);
const duration = parseInt(__ENV.DURATION || '60', 10);
const killAt = parseInt(__ENV.KILL_AT || '0', 10);
const params = { headers: { Authorization: `Bearer ${__ENV.API_KEY || 'e3-key'}` } };

function phase() {
  const t = exec.instance.currentTestRunDuration / 1000;
  if (t < warmup) return 'warmup';
  if (killAt > 0) return t < killAt ? 'before' : 'after';
  return 'measure';
}

const thresholds = { 'dropped_iterations': ['count>=0'] };
for (const p of ['measure', 'before', 'after']) {
  thresholds[`http_reqs{phase:${p}}`] = ['count>=0'];
  thresholds[`http_reqs{phase:${p},status:200}`] = ['count>=0'];
  thresholds[`http_reqs{phase:${p},status:429}`] = ['count>=0'];
  thresholds[`http_req_duration{phase:${p}}`] = ['max>=0'];
}

export const options = {
  discardResponseBodies: true,
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  scenarios: {
    load: {
      executor: 'constant-arrival-rate', rate, timeUnit: '1s', duration: `${warmup + duration}s`,
      preAllocatedVUs: Math.max(50, Math.ceil(rate / 20)), maxVUs: 2000,
    },
  },
  thresholds,
};

export default function () {
  const t = targets[exec.scenario.iterationInTest % targets.length];
  http.get(`${t}/e3`, Object.assign({ tags: { phase: phase() } }, params));
}

export function handleSummary(data) {
  return { [__ENV.SUMMARY || 'summary.json']: JSON.stringify(data) };
}
