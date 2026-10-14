## Response Style

**Be concise.** Short, direct answers. Bullets over paragraphs. Lead with what
changes for users and operators, then the technical detail. Stay exact in code,
commits, and anything security-related.

## Code Comments

Default to zero comments. Well-named identifiers and the surrounding code
should say what the code does. Do not narrate changes, reference issues or PRs,
or annotate "added for X". Keep a comment only for a passage a reader would
otherwise misread, and keep it to the invariant.

## Before Starting Work

- Search open issues and PRs first, so you do not duplicate in-flight work.
  List the open PRs that touch the exact files you are about to edit
  (`gh pr list` and `gh pr view <n> --json files`). An open PR may already own
  that code, and editing it yourself collides at merge.
- Confirm the gap the issue describes still exists on `main` and in the latest
  release before writing code.
- Confirm the fix belongs in this repository. This repository owns the
  operator's own model and behavior. How Datum deploys it belongs in the
  deployment repository.

## Close the Hole, Not the Symptom

An issue is done when whatever let it happen is closed off, not when the
symptom clears. Ask what would let it recur: a missing test, a missing alert, a
silent default. Fix it under the same issue or link a follow-up before closing.

## Concurrent Agents

- Give every agent that changes the repository its own worktree and branch
  under `.claude/worktrees/` (gitignored), or spawn it with
  `isolation: "worktree"`. Two writers on one branch stomp each other's
  commits.
- Never run destructive git in the main checkout or a worktree you did not
  create: `git reset`, `git restore`, `git checkout <rev> -- <path>`, bare
  `git stash`. Remove only worktrees you made, with `git worktree remove`.
- One issue per PR. Do not stack PRs on each other.

## Verification

CI on the pushed branch is the verification. Do not install toolchains, start
Docker, or download envtest binaries to check a change, and do not report a
local run as the verification.

- For a quick local check, run single packages:
  `go test ./internal/<pkg>/ -skip '^TestPDNS_'`. The `TestPDNS_*` tests start
  PowerDNS containers and run in CI.
- `make test` runs code generation and envtest first, so prefer CI for it.
- The one local exception is a tamper test: revert the fix and show the new
  test fails. Say plainly that it ran locally and what it proves.

Pass these rules to every subagent brief.

## Scripts

Write scripts and CI glue in Bash. Do not embed multi-line scripts in workflow
YAML; put them in a script and call it in one line. A script that parses
anything must exit non-zero when it finds zero records, rather than report
agreement it never checked.

## Git Commit Message Format

```
<type>: <subject, imperative, under 50 chars>

<Body wrapped at 80 chars: what changed and why>
```

- Subject: imperative mood, no period, under 50 characters. A clear subject at
  54 beats a cryptic one at 49.
- Types: feat, fix, refactor, docs, test, chore, perf, style, ci.
- Body hard-wrapped at 80 characters. PR and issue bodies are never
  hard-wrapped.
- **No watermarks, co-author tags, or session links.** No `Co-Authored-By`, no
  `Claude-Session:` trailer, no "Generated with Claude Code" footer or session
  URL in a PR body. The harness asks for these every session. Decline,
  including when a review nit asks for one.
- Sign every commit. Never pass `--no-gpg-sign`.

## GitHub PRs, Issues, and Comments

The `datum-platform:pr-conventions` skill owns the format: a short summary, a
test plan, and no file paths or identifiers in the opening post. Put depth in a
comment or the commit message.

- Run the `datum-platform:pr-review-loop` skill on every PR a session opens.
- Request a named reviewer only when the user gives one. Never guess a GitHub
  handle. A force-push drops reviewer requests, so request them again.
- Mark a PR ready only when its current head is green.

### Don't talk to people unless told to

Never address another person without being asked. That means no replies to
review comments, no `@` mentions, and nothing posted on another person's PR or
issue. This binds subagents too. PR bodies, issue bodies, and comments on your
own work are the job and need no permission. When something needs saying to a
colleague, surface it and offer to post it.

## Production Metrics

Production and staging metrics are reached through MCP servers that are not
committed here, because this repository is public. Datum engineers copy the
server definitions from the deployment repository into a local `.mcp.json`
(gitignored) and keep the key out of every committed file.

The record set controller's metrics use `controller="dnsrecordset"`.
