// Creates, updates and deletes private TXT record sets through the project API
// at a fixed rate, for publish.js to watch from inside the VPC. Runs on the
// operator's machine; publish.sh starts both.
import http from 'k6/http';
import exec from 'k6/execution';

const env = (k, d) => (__ENV[k] === undefined || __ENV[k] === '' ? d : __ENV[k]);
const API = env('API', '');
const TOKEN = env('TOKEN', '');
const PREFIX = env('PREFIX', '');
const ZONE_NAME = env('ZONE_NAME', 'idns-perf');
const COUNT = parseInt(env('COUNT', '20'), 10);
const RATE = Number(env('WRITE_RATE', '2'));
const START_MS = Number(env('START_MS', '0'));
const GAP_S = parseInt(env('GAP_S', '30'), 10);
const TTL = parseInt(env('TTL', '1'), 10);
if (!API || !TOKEN || !PREFIX || !START_MS) throw new Error('API, TOKEN, PREFIX and START_MS are required');

const base = `${API}/apis/dns.networking.miloapis.com/v1alpha1/namespaces/default/dnsrecordsets`;
const phase = Math.ceil(COUNT / RATE);
const offset = Math.max(0, Math.round((START_MS - Date.now()) / 1000));
// Whole-second rates only; fractional rates use a longer time unit.
const unit = RATE >= 1 ? '1s' : `${Math.round(1 / RATE)}s`;
const perUnit = RATE >= 1 ? RATE : 1;

function step(name, n, fn = name) {
  return {
    executor: 'constant-arrival-rate',
    rate: perUnit,
    timeUnit: unit,
    // A whole-second phase can end one arrival short; the extra iterations are skipped.
    duration: `${phase + 2}s`,
    startTime: `${offset + n * (phase + GAP_S)}s`,
    preAllocatedVUs: Math.max(5, Math.ceil(RATE * 2)),
    maxVUs: Math.max(20, Math.ceil(RATE * 10)),
    exec: fn,
    tags: { step: name },
  };
}

export const options = {
  scenarios: { create: step('create', 0), update: step('update', 1), delete: step('delete', 2, 'remove') },
  summaryTrendStats: ['med', 'p(90)', 'p(99)', 'max'],
};

const headers = (type) => ({ headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': type } });

function send(stepName, fn) {
  const index = exec.scenario.iterationInTest;
  if (index >= COUNT) return;
  const name = `${PREFIX}-${index}`;
  const sent = Date.now();
  const res = fn(name, sent);
  console.log(`WRITE ${JSON.stringify({ index, step: stepName, sent, done: Date.now(), status: res.status })}`);
}

const records = (name, version, sent) => [{ name, ttl: TTL, txt: { content: `v=${version} t=${sent}` } }];

export function create() {
  send('create', (name, sent) =>
    http.post(
      base,
      JSON.stringify({
        apiVersion: 'dns.networking.miloapis.com/v1alpha1',
        kind: 'DNSRecordSet',
        metadata: { name, labels: { 'test.datum.net/owner': 'internal-dns-perf', 'test.datum.net/run': PREFIX } },
        spec: { dnsZoneRef: { name: ZONE_NAME }, recordType: 'TXT', records: records(name, 1, sent) },
      }),
      headers('application/json'),
    ));
}

export function update() {
  send('update', (name, sent) =>
    http.patch(`${base}/${name}`, JSON.stringify({ spec: { records: records(name, 2, sent) } }), headers('application/merge-patch+json')));
}

export function remove() {
  send('delete', (name) => http.del(`${base}/${name}`, null, headers('application/json')));
}
