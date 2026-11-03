// Exercise the actual suite metadata without needing the xk6 DNS extension.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const samples = [];
class Metric {
  constructor(name) { this.name = name; }
  add(value, tags) { samples.push({ name: this.name, value, tags }); }
}
const context = {
  __ENV: { PRIVATE_NAMES: 'target.instances.datum.internal', PUBLIC_RATE: '0' },
  Counter: Metric, Gauge: Metric, Rate: Metric, Trend: Metric,
  dns: { resolve: async () => ['fd20::1'] }, sleep: () => {},
  exec: { scenario: { startTime: 1793049000000, iterationInTest: 0 } }, console,
};
let source = fs.readFileSync(__dirname + '/dns.js', 'utf8');
source = source.replace(/^import .*;$/gm, '').replace(/^export /gm, '');
vm.runInNewContext(source + '\nglobalThis.test = { queryPrivate, handleSummary };', context);
(async () => {
  await context.test.queryPrivate();
  await context.test.queryPrivate();
  assert.equal(samples.filter((s) => s.name === 'idns_scenario_start_ms').length, 1);
  assert.equal(samples.find((s) => s.name === 'idns_scenario_start_ms').value, 1793049000000);
  const result = context.test.handleSummary({ metrics: {}, state: { testRunDurationMs: 120001 } });
  const metadata = JSON.parse(result['/tmp/k6-summary.json']).idns;
  assert.equal(metadata.version, 1);
  assert.equal(metadata.resolver, '[fd53::53]:53');
  assert.equal(metadata.scenarios.private.rate, 150);
  assert.equal(metadata.scenarios.private.duration, '2m');
  console.log('k6 scenario start and summary metadata verified');
})().catch((error) => { console.error(error); process.exitCode = 1; });
