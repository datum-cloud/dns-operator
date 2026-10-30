# CI foundation and DNS deployment

The shared foundation consists of three dual-stack kind clusters (`dns-control`,
`dns-upstream`, and `dns-edge`) with Flux, cert-manager, Kyverno, and Envoy Gateway.
Prepare it once, then deploy the code under test:

```sh
task --yes env:foundation-up
task --yes env:internal-dns:deploy
task --yes env:stack-deploy
```

The deployment commands require prepared clusters. They do not create clusters or
reinstall foundation components. Public deployment retains control → edge →
upstream ordering because the later deployments need RustFS and control API
connection details. DNS images, DNS CRDs, RustFS, and test fixtures remain in the
deployment phase, so the foundation does not capture the PR's DNS implementation.

`task e2e` prepares the foundation once and runs both deployments and suites.
`env:internal-dns:up` and `env:stack-up` remain standalone convenience commands
that prepare the foundation before deploying their respective DNS workloads.
When combining workloads, use the explicit sequence above instead of calling
both convenience commands. `env:down` still deletes all three clusters.
Existing IPv4-only clusters must be recreated before private DNS deployment.

The E2E workflow measures harness tests, foundation preparation, private and public
DNS deployment, private qualification, and Chainsaw separately. Each phase prints
its duration and exit status and adds them to the GitHub job summary. The
`e2e-phase-timings` artifact contains `ci-timings.jsonl` records with `phase`,
`seconds`, and `exit_code`; failed commands retain their exit status and record
partial durations. An abruptly cancelled runner may not finish writing a record.

A successful cold `foundation` duration is the maximum preparation time that a
ready warm pool could avoid. Actual savings must subtract environment acquisition
and readiness checks. DNS build/deployment and tests still run for every PR.
This change measures that opportunity; it does not introduce a warm pool or skip
foundation readiness checks.

Run timing-helper checks with:

```sh
python3 -m unittest discover -s hack -p 'test_time_ci_phase.py'
```
