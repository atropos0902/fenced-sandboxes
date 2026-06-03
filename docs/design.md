# Design

## The problem that shaped it

A worker stops answering. Another worker takes the run and starts a new sandbox. The first sandbox was only slow, or it was paused, and it comes back with a finished change and a working token. Both hosted providers can resume a paused machine with its processes still running, so deleting the old sandbox is necessary and it is not enough. The delete call can fail too.

So ownership is a number in Postgres. Claiming a run adds one to `runs.epoch`, and a sandbox token works only while its epoch is the run's epoch. Deleting the old sandbox saves money. Nothing depends on it for safety.

## Pushes

A check followed by a push has a gap in the middle. The gate closes it by holding the run's row lock from the check until the branch has been read back. Claims use `SKIP LOCKED`, so a worker looking for work passes over a run with a push in progress and gets it a moment later.

Holding a database lock across a network call is usually a mistake. It is bounded here: the pack is written to disk before the lock is taken, the call has 20 seconds, and a lease has to be longer than that or the process refuses to start.

The answer to a push is not trusted. A 200 can carry a refusal and a timeout can hide a success, so the gate reads the branch. If the branch can't be read, or a failed call leaves it where it was, the run goes to `attention` and stays there. The second case matters, the request may still be on its way.

Two limits. If Postgres loses the commit after GitHub took the push, the recorded head is behind, and the next push is refused and stops the run. And a person with write access can move the agent branch by hand, which has the same effect.

## What a sandbox holds

One token, for one attempt, with named capabilities. It is passed per command and is not in the image.

| | fetch | push | outbound HTTPS | model keys |
|---|---|---|---|---|
| build | yes | no | yes | no |
| agent | yes | own branch | yes | yes |
| verify | yes | no | yes | no |

Model keys stay in the gate, which swaps the token for the key on a fixed upstream. An agent can still spend the key's money while its attempt is live. There is no budget per run yet, and there should be.

GitHub tokens are minted per repository without the `workflows` permission. Pushing to a branch starts that branch's workflows at once, so checking for a changed workflow file after the push would be too late.

## Two sandboxes, not one

Checks and the preview run in a second sandbox at the pushed commit. In the agent's own sandbox, a check can pass because of a file nobody committed, or because the agent changed the test runner. The cost is one more start, which is about a third of a second when setup inputs did not change.

The preview's token is revoked before the app starts. Each run's preview has its own host name, and the viewer's link is tied to the commit.

## Starting quickly

A snapshot is keyed by the provider, the image, the setup command and the blob ids of the files setup reads. The ids come from GitHub. If a sandbox computed the key, an agent could choose which snapshot the next run starts from.

Snapshots are built from the base branch only, and the builder's token is revoked before the copy is taken. Setup scripts write all sorts of things to disk, `.npmrc` is the usual one.

Two runs that miss the cache together both build. The second image is wasted, and that is cheaper than making one wait on a builder that might die.

## Containers or VMs

Docker is for development and tests. A container shares the host's kernel, and a kernel bug is a way out. gVisor helps where it is installed (`DOCKER_RUNTIME=runsc`) and slows system calls, which installs notice. For customers' code the choice is a microVM: E2B runs Firecracker, and Blaxel documents microVMs without naming the hypervisor.

| | Docker | E2B | Blaxel |
|---|---|---|---|
| Unique names | yes | no, create looks up its own label first | yes |
| Start from a snapshot | yes | yes | no |
| Can be resumed after it looked finished | paused only | yes, auto-pause is turned off here | yes, standby is the default |
| Outbound limit | internal network, one per sandbox | address list | domain list in its proxy |
| Memory evidence | cgroup counter | exit status only | exit status only |

E2B's older list call returns running sandboxes only. The adapter uses the one that can include paused, or a leaked paused sandbox would never be found.

On one Docker host the number of sandboxes is limited by address pools (about 30 networks by default), well before memory. Overlay storage has no per-container disk limit here either. Neither applies to the hosted providers.

## Explaining failures

Measured facts come first: the cgroup's OOM count, hosts the proxy refused during that phase, whether the provider still has the sandbox, which address the port is open on. Log patterns come after. An installer that retries a blocked host until time runs out is reported as the blocked host.

Each cause has an owner. Only platform causes are retried, since running a broken lockfile three times gives three broken lockfiles. A process killed by a signal is checked against the provider's list before it is blamed on the agent.

## Cleaning up

The ledger row is written before the provider is called, so a create that timed out is still known. The reconciler reads the provider's list, then the ledger, and does nothing if either read fails. Sandboxes younger than the grace period are left alone. The provider's own time limit is set on every sandbox and works when all of this is down.
