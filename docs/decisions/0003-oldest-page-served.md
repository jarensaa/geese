# Decision 0003: Offline: the app keeps itself, and the servers say which releases they still serve

> **In geese's words.** This decision was made while the coordinator lived inside an application, and is kept as it was argued. Its pods are geese; a pod's lease is a goose's place in the flock, kept by honking (`geese_flock`, ex `schema_instances`); the upgrade fence is the flock's heading (`geese_flight`, ex `schema_upgrade_state`); draining is gathering; the deployment gate is the weather; the migration policy is the flight plan. Other ADR numbers are the application's own decisions.

Status: Accepted: the service worker keeps the app, and the deployment says the oldest release whose page it serves. Keeping a service's data on the device for offline use is left to the service (see *Data on the device*). Builds on ADR 0007 and amends [decision 0002](0002-capabilities.md).

## Context

People open an installed app where the signal is poor, and expect it to open at once. Without a connection, an app whose service worker caches nothing opens to nothing.

There's a reason not to cache. The page and the API change together, and several images serve one database through live upgrades ([decision 0001](0001-live-schema-migrations.md)). New behavior waits for a capability ([decision 0002](0002-capabilities.md)), but a release that changes or removes an operation (`feat!`) leaves an older page speaking an API that's gone, and nothing tells that page so. A tab left open across a release has that problem already; a cached app has it every time it's opened.

## Decision

### The app keeps itself

#### The service worker keeps the release it comes with

- The web build carries its release (`APP_VERSION`, as the server's does). Once the build is out, it writes into `sw.js` the release, an id from everything in the build, and the list of its files. That also changes the worker every build, so browsers install the new one. The development server's worker has no files, and keeps nothing.
- Installing keeps the whole build or nothing. During a rollout, a file asked of a pod still on the previous image isn't found, so the install fails and the worker before stays, to be tried again later. A worker that has installed takes over at once. It keeps the previous build beside its own, for pages still open on it that ask for a file they haven't loaded yet, and deletes older ones.
- The app opens at its own address and routes by the hash (`#workspace=…`, `#invite=…`), so only a navigation to `/` is answered with the kept page. Every other path, such as `/auth`, `/api`, `/mcp` or `/.well-known`, is the server's, and so is every API request. Files under `/assets/` are named by their content, so any kept build's copy will do; the page, the manifest and the icons come from this build.

So the app opens at once from the device, with a connection or without. Opened without one, it says *You're offline* and signs in by itself as the connection returns.

#### The deployment says the oldest release whose page it serves

`GET /api/capabilities` gains `oldestPage`, the first release of the oldest major the deployment serves pages of, for example `"8.0.0"`. It's absent until one is set.

- **Why a major.** A release that breaks an existing page is a major release: semantic-release makes `feat!` and `BREAKING CHANGE` majors. A page of an older major may call what no server has any more. Majors that break nothing, like a schema baseline, raise it too. That's harmless: kept apps load the current release the next time they're online.
- **Agreed across the cluster.** Each pod's lease records its release's major (`schema_instances.release_major`). The leader raises the floor (`schema_upgrade_state.oldest_page_major`) to the oldest major every live pod is on, once the deployment has settled, under the same lock as capabilities ([decision 0002](0002-capabilities.md)). During a breaking rollout it waits for the last pod of the old major, so all pods answer the same floor and it never goes down. A pod that declares no release (one from before this, or a build that isn't a release) holds it where it is.
- **Rollback.** An image below the floor is refused at admission, as for an active capability. A build that isn't a release can't say which pages it serves, so it's refused too once a floor is set.
- **Every image is a release.** A `main` build that isn't a release carries the latest release with its commit as build metadata (`8.1.0+<commit>`): nothing breaking landed since, or it would be a release, so it serves that release's pages. Before, it was `0.0.0-dev.<commit>`, which says nothing.

This amends decision 0002, which kept versions out of the capability endpoint. The floor isn't the schema or a pod's version: it's the oldest page the deployment promises to serve, the one thing a kept page can't learn by itself.

#### A page older than that loads the current release

- **When it reads the floor.** With the capabilities: as the app opens, and again as it comes back into view (at most once a minute). An answer that could mean the server no longer has what the page asked for (400, 404, 405, 410) reads them again, at most every 30 seconds. Changes wait for the first read, so a page opened from an older release never sends one first. Reads don't wait: opening stays as fast as before, and a read misunderstood is shown, not saved.
- **What it does below it.** It stops calling the API: its client refuses every request, and the page shows only *Monolith has been updated* in place of itself, which closes the workspace's live socket. It asks the worker for a newer release and opens again once the worker keeps one. A worker that keeps none (or, from before this, doesn't say) leaves opening to the server. If none is to be had yet, the page says so and tries again every 30 seconds, as the connection returns, and on *Try again*. It opens itself again at most once every 30 seconds, so a page that opens just as old, say from a cache between it and the server, waits instead of reloading on.
- **Offline, it can't know and doesn't ask.**

#### Kept honest in CI

A pull request that changes `openapi.yaml` in a way [oasdiff](https://github.com/oasdiff/oasdiff) finds breaking (an operation, parameter or field removed, a type narrowed, a required request field added) fails unless it's marked breaking where the release will be read from: its title, for a squash merge, and a commit, for a merge commit. oasdiff is a pinned release, checked against its published checksum. A change of meaning no diff can see is for review, as before.

#### Rollout

No migration. The coordination tables gain the two columns additively, outside Goose, as capabilities did. Pods from before declare no release, so the floor waits until the last of them is gone. The first release with this sets the floor to its own major once it alone serves. Until then `oldestPage` is absent, and no page is told it's out of date.

### Data on the device

The app keeps itself, not the service's data: offline, it opens and says it's offline. A service that needs its data offline keeps the reads its pages already make, read-only, and forgets them as the servers say: a change number moved on (ADR 0005), access gone (403, 404, the socket closing for members), someone else signed in, `Cache-Control: no-store`, or a release floor raised. Which data may be kept, and for how long, is the service's own decision: terms of the data's sources may forbid keeping it on a device at all.

## Consequences

- The installed app opens at once, online or not. A release the servers still serve keeps running until the next launch, so a new feature reaches people one launch later.
- A tab left open across a breaking release learns it on its next answer it didn't expect, or as it comes back into view, and loads the current page. That was broken before.
- A breaking release raises the floor once it alone serves; a rollback below it is refused, as for a capability.
- The browser tests build the app as release `1.0.0`, and a fixture tells one browser the servers serve pages from a later release.

## Alternatives considered

- **A cache-first app without a floor, the usual PWA.** Online people would run whatever build was kept until the next launch, with nothing to tell them it no longer works. That's the problem ADR 0007 declined to take on.
- **Network first for the app, with a kept copy that never calls the API.** It avoids the floor, but opens slowly on one bar, and a tab left open across a breaking release stays broken. The floor fixes both.
- **A contract number in `openapi.yaml`, bumped by hand.** The same signal as the major release, kept by people instead of the release process. Two numbers that both mean "breaking" will disagree.
- **Capabilities alone.** A capability says a feature has started; nothing says an old page has stopped working.
- **The floor from each pod's own release, not agreed.** During a rollout pods would disagree, and a page sent to load the current release could load the old one from another pod, back and forth.
- **Every request waiting for the floor.** It would add a round trip to every launch, slowest exactly on one bar. Only changes wait.
- **The service worker caching `/api` responses (stale-while-revalidate).** It caches below the page, so it can't tell an offline read from an online one. An online page would get stale answers beside the live socket's change numbers, and nothing would decide what may be kept.
- **A native app.** Rejected in ADR 0007, and a web app can do this.
