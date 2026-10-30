# Validate recovery before fixture cleanup

Keep positive DNS fixtures and their consumer probe pods until the restored fleet
has passed this check. A Ready pod or an old activation timestamp alone does not
prove that the desired configuration is serving. This command only reads state,
executes bounded DNS probes, and writes evidence. It never deletes fixtures or
runs a cleanup command. A nonzero exit means **retain the fixtures** and investigate.

Run this after the rollout or rollback has settled, with no concurrent operator
changes. Use the restored image containing the `activation-check` role. Older
images fail this gate rather than falling back to a weaker readiness check.

```sh
test/internaldns/validation/before-cleanup.sh expectations.json /absolute/new-evidence-dir
```

Only proceed with a separately authorized, explicitly scoped cleanup if this
invocation exits zero. Do not reuse an old pass result: rerun immediately before
cleanup after any delay or configuration change. The gate accepts at most 60
seconds of validation work. There is no atomic transaction between Kubernetes,
DNS, and a later cleanup; a passing result is a bounded observation, not a lease
or authorization to remove resources.

The specification names every affected member and positive fixture. Include the
node resolver and any regional members used by the restored path. It cannot
infer an omitted member. A probe runs inside an existing consumer pod, not the
serving pod. Include each expected positive fixture using its exact A or AAAA
RRset. CNAME chains, negative answers, truncation, and extra answers fail rather
than being treated as proof of recovery. IPv6 values must use dig's textual
normalization (for example, `fd20::1`).

```json
{
  "context": "us-east-1-staging-lab",
  "members": [{
    "namespace": "serving-namespace",
    "pod": "expected-restored-pod",
    "uid": "expected-pod-uid",
    "container": "agent",
    "config": "/etc/internal-dns/config.json",
    "config_sha256": "REPLACE_WITH_64_HEX_DIGITS",
    "images": {
      "agent": "ghcr.io/example/serving@sha256:REPLACE_WITH_64_HEX_DIGITS",
      "serving": "ghcr.io/example/serving@sha256:REPLACE_WITH_64_HEX_DIGITS",
      "dnsdist": "ghcr.io/example/dnsdist@sha256:REPLACE_WITH_64_HEX_DIGITS"
    },
    "configmap": {
      "name": "expected-configmap",
      "uid": "expected-configmap-uid",
      "sha256": "REPLACE_WITH_64_HEX_DIGITS"
    }
  }],
  "probes": [{
    "namespace": "consumer-namespace",
    "pod": "retained-consumer-pod",
    "uid": "expected-consumer-uid",
    "container": "k6",
    "resolver": "fd53::54",
    "name": "retained-target.instances.datum.internal.",
    "type": "AAAA",
    "answers": ["fd20::1"]
  }]
}
```

Prepare expected images, configuration and fixture values from the approved
rollback plan, not by accepting whatever is currently running. `images` includes
all regular containers. `config_sha256` is the SHA256 of the exact mounted JSON
configuration bytes. The ConfigMap checksum is calculated using
`jq -S -c .data configmap.json | shasum -a 256` (including jq's final newline).
UIDs prevent a replacement object being accepted under the same name. Secrets
are never fetched; evidence includes pod objects and the named ConfigMap, so
store it with the same access controls as that configuration.

The strict role compares the latest **locally accepted** snapshot and attached
publications with the approved checkpoint, requires no activation marker, checks
installed files and current runtime settings, and performs the existing live
readiness canaries. It checks state again after those probes and prints a hash
of the approved activation. This does not prove delivery of an event the agent
has not received; the operator must also establish current control-plane delivery
and expected publication during rollout validation.

The wrapper checks UID, immutable images, mounted configuration and this token
before and after exact consumer UDP/TCP queries. A changed token, pending
publication, stale configuration, malformed or empty specification, failed exec,
failed probe, or elapsed deadline produces no pass. This is an operational
recovery check, not a replacement for immediate tenant withdrawal or deletion:
security revocations must never wait for positive fixture validation.

Local deterministic fixtures run with:

```sh
bash test/internaldns/validation/test-before-cleanup.sh
go test ./internal/internaldns/serving -run 'Test(CurrentActivation|ActivationCheck)'
```

The Go wrapper includes the shell fixtures in the normal CI test suite.
