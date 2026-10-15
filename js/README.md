# @jarensaa/geese

A page's side of a [geese](../README.md) flock: what the deployment has switched on, and moving to the current release once the servers no longer serve this one. No framework, no dependencies.

```ts
import createClient from 'openapi-fetch'
import { createAppUpdate, createDeployment } from '@jarensaa/geese'

export const api = createClient<paths>({ credentials: 'same-origin' })

// What the flock says, from the API's capabilities endpoint: {capabilities, oldestPage}.
export const deployment = createDeployment({
  read: async () => (await api.GET('/api/capabilities')).data,
})

// This page's release, as its build set it ("dev" for a build that isn't one).
export const update = createAppUpdate({ release: import.meta.env.APP_RELEASE ?? 'dev', deployment })
api.use(update.middleware)

deployment.watch() // read as the page opens, and again as it comes back into view
update.watch()     // move on when the flock no longer serves this release
```

With React:

```ts
const comments = useSyncExternalStore(deployment.subscribe, () => deployment.has('comments'))
const state = useSyncExternalStore(update.subscribe, update.state) // 'current' | 'updating' | 'stuck'
```

- **Capabilities.** `deployment.has(name)` is false until the flock has been read and has switched it on. A feature waits for it.
- **The oldest page served.** Once the flock serves pages only from a later major release than this page's, the page stops calling the API (requests fail with `OutdatedPageError`) and loads the current release. With a service worker, it asks the worker for a newer release first, and opens again once the worker keeps one; until it can, `update.state()` is `'stuck'`, and it tries again every thirty seconds, as the connection returns, and on `update.tryAgain()`.
- **Changes wait for the first read**, so a page opened from an older release never sends one before it knows it may. Reads don't wait.
- **An answer the page didn't expect** (400, 404, 405, 410) reads the flock again, at most every thirty seconds: the server may no longer have what it asked for.
- Without openapi-fetch, wrap fetch instead: `const guarded = update.fetch(fetch)`.

### The service worker

A worker that keeps the app answers which release it keeps, so a page knows whether opening again would load a newer one:

```js
self.addEventListener('message', (event) => {
  if (event.data?.type === 'release') event.ports[0]?.postMessage({ release: BUILD.release })
})
```

A page without a worker, or with one that doesn't answer, leaves opening the current release to the server.
