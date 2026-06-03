# Runbook

Every log line carries `run` and `epoch`. `GET /runs/{id}/events` is the history of one run in order, and it is usually enough.

## Reconciler stalled

Nothing is cleaning up. The provider's list call is the usual cause, because the reconciler refuses to act on a partial list. Look for `reconcile` errors in the control log. Sandboxes still end at the provider's own time limit, so cost is capped while you look. Do not delete sandboxes by hand from the provider's console unless you also have the run's id, a live review looks the same from there.

## Sandboxes leaking

The reconciler is removing them, so this is about where they come from. `leak_removed` events name the sandbox and the epoch. An epoch lower than the run's means takeovers are failing to destroy the old sandbox: check delete errors. A name with no row at all means something outside this service is using our owner label.

## Platform failures

Group `fenced_failures_total` by `code`.

- `provider_create_failed`: quota or a provider incident. Runs retry up to `max_attempts` and then fail, so stop new runs before the queue burns through its attempts.
- `sandbox_lost`: the provider ended sandboxes early. Compare with its time limit.
- `push_failed`: read the `push` phase output. GitHub refuses pushes that touch `.github/workflows`, which is intended, and it shows up here.

## Slow warm start

Check the `clone` and `setup` phases separately. If `setup` appears on warm starts at all, the cache key is missing and the log says `no cache key`. If `clone` is slow, the upstream is.

## Stale push refused

A replaced sandbox tried to push and was stopped at the lock. One of these is the design working. Several at once mean leases are ending while workers are still alive, so look for database stalls longer than a lease.

## A run in `attention`

A push was sent and we could not find out whether it landed. Read the branch on GitHub. If it is at the commit you expect, open the pull request by hand, otherwise cancel the run and start another. Nothing in this state is retried by itself.
