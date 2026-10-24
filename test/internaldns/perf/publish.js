// Watches the private records publish-writer.js creates, updates and deletes,
// from inside a compute instance, and prints when each change was first
// answered by the VPC resolver. See README.md, "Publishing".
//
// One VU follows one name through its life: absent, then "v=1 t=<ms>", then
// "v=2 t=<ms>", then NXDOMAIN again. The writer stamps each value with the time
// it sent the request, so the prober can report the lag without sharing state.
// Deletes carry no stamp; publish.sh joins them with the writer's log.
import dns from 'k6/x/dns';
import { sleep } from 'k6';
import { Trend, Counter } from 'k6/metrics';
import exec from 'k6/execution';

const env = (k, d) => (__ENV[k] === undefined || __ENV[k] === '' ? d : __ENV[k]);
const RESOLVERS = env('RESOLVERS', '[fd53::53]:53').split(',').map((s) => s.trim()).filter(Boolean);
const PREFIX = env('PREFIX', '');
const ZONE = env('ZONE_DOMAIN', 'perf.internal');
const COUNT = parseInt(env('COUNT', '20'), 10);
const RATE = Number(env('WRITE_RATE', '2'));
const START_MS = Number(env('START_MS', '0'));
const POLL_MS = parseInt(env('POLL_MS', '200'), 10);
// Gives up on a name once a step has waited this long.
const STEP_TIMEOUT_MS = parseInt(env('STEP_TIMEOUT_MS', '120000'), 10);
if (!PREFIX || !START_MS) throw new Error('PREFIX and START_MS are required');

const lagMs = new Trend('idns_publish_lag_ms', true);
const missed = new Counter('idns_publish_missed');

export const options = {
  scenarios: {
    watch: {
      executor: 'per-vu-iterations',
      vus: COUNT,
      iterations: 1,
      // Every step can time out; leave room for all three plus the writer's schedule.
      maxDuration: `${Math.ceil(COUNT / RATE) * 3 + (3 * STEP_TIMEOUT_MS) / 1000 + 120}s`,
    },
  },
  summaryTrendStats: ['med', 'p(90)', 'p(99)', 'max'],
};

function emit(o) {
  console.log(`PUBLISH ${JSON.stringify(o)}`);
}

async function lookup(name, resolver) {
  try {
    const values = await dns.resolve(name, 'TXT', resolver);
    return values.find((v) => v.startsWith('v=')) || '';
  } catch (e) {
    const s = String((e && e.name) || e);
    if (s.includes('NonExistingDomain')) return null;
    return undefined;
  }
}

// Polls until want(value) holds on every resolver, and reports each resolver's
// first matching answer on its own line.
async function waitFor(index, name, step, want) {
  const deadline = Date.now() + STEP_TIMEOUT_MS;
  const pending = new Set(RESOLVERS);
  let errors = 0;
  while (pending.size > 0 && Date.now() < deadline) {
    for (const resolver of [...pending]) {
      const value = await lookup(name, resolver);
      if (value === undefined) {
        errors++;
        continue;
      }
      if (want(value)) {
        const seen = Date.now();
        const stamp = value ? Number((value.match(/t=(\d+)/) || [])[1]) : 0;
        if (stamp) lagMs.add(seen - stamp, { step, resolver });
        emit({ index, name, step, resolver, seen, stamp, errors });
        pending.delete(resolver);
      }
    }
    if (pending.size > 0) sleep(POLL_MS / 1000);
  }
  for (const resolver of pending) {
    missed.add(1, { step, resolver });
    emit({ index, name, step, resolver, seen: 0, stamp: 0, errors });
  }
}

export default async function () {
  const index = exec.vu.idInTest - 1;
  const name = `${PREFIX}-${index}.${ZONE}`;
  // Start looking when the writer is due to create this name, so the wait for
  // the create is not inflated by a negative answer cached long before.
  const due = START_MS + (index / RATE) * 1000;
  if (due > Date.now()) sleep((due - Date.now()) / 1000);
  await waitFor(index, name, 'create', (v) => v !== null && v.startsWith('v=1 '));
  await waitFor(index, name, 'update', (v) => v !== null && v.startsWith('v=2 '));
  await waitFor(index, name, 'delete', (v) => v === null);
}
