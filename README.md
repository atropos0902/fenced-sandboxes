# Fenced sandboxes

Runs a coding agent against a repository in a throwaway sandbox and turns the result into a pull request with a live preview. The agent's code is not trusted, and neither is the sandbox after it has been replaced.

```
scripts/dev.sh up -d          # Postgres, a stand-in for GitHub, the gate, the control process
scripts/try.sh "sed -i 's/Shop/Corner shop/' src/server.ts"
```

The second command prints the run, its history and a preview link. No accounts are needed: the stand-in speaks the GitHub calls this uses, and real git over HTTP.

## How it fits together

`fenced control` has the API, the workers, the reconciler and the provider keys. `fenced gate` is what a sandbox talks to: git, model APIs, outbound HTTPS and the preview. Both use Postgres and share nothing else. The gate has no Docker socket and no provider key, so breaking into it does not give anyone a new sandbox.

A run has an epoch. Claiming a run raises it. Every token, every push and every state change carries the epoch it was made under, and an old one changes nothing. [docs/design.md](docs/design.md) has the reasons and the limits. [docs/runbook.md](docs/runbook.md) goes with the alerts in `deploy/alerts.yml`.

## Tests

`go test ./...` needs `TEST_DATABASE_URL` for the gate tests, which use the real git client and real `git-http-backend`. `scripts/e2e.sh` runs the stack in Docker and breaks it: killed worker, killed sandbox, a replaced sandbox that wakes up and pushes, memory limit, blocked host, bad previews.

Twenty-four of the checks in the gate and the store were removed one at a time. A test failed each time.

## What has not been run

The E2B and Blaxel adapters are tested against servers written from their published API descriptions. They have not been run against the live services, and neither has the GitHub client. The GitHub Actions workflow has not run either. Every number below is Docker with runc on a machine with 4 cores.

## Measured

| | |
|---|---|
| Sandbox ready, environment prepared earlier | 0.27 to 0.39 s, 10 starts |
| Sandbox ready, agent changed a setup input | 1.9 to 2.3 s, 5 starts |
| Clone and `npm ci` from nothing (98 MB) | 4.4 to 24 s, 5 builds |
| Worker killed, run ready for review again | 37 s, with a 25 s lease and an 8 s agent |

The wide range in the third row is the npm registry. One build took 5 seconds and the next took 24.
