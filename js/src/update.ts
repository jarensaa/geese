// Moving a page to the current release once the flock no longer serves its
// own. A service worker may open the page from the release it keeps, so a page
// may be older than the servers: the flock says the oldest release whose page
// it serves, and a page older than that stops calling the API and loads the
// current one.
import type { Deployment } from './deployment.ts'
import { belowOldestPage, unexpectedAnswer } from './release.ts'

/**
 * - current: the flock serves this page, or hasn't said otherwise.
 * - updating: it doesn't; the current release is on its way.
 * - stuck: it doesn't, and the current release couldn't be loaded yet: tried again shortly.
 */
export type AppUpdateState = 'current' | 'updating' | 'stuck'

/** A request this page didn't send, because the servers no longer serve its release. */
export class OutdatedPageError extends Error {
  constructor() { super('This page’s release is older than the flock serves') }
}

type Timers = { setTimeout: (callback: () => void, ms: number) => unknown; clearTimeout: (handle: unknown) => void }

export type AppUpdateOptions = {
  /** The release this page is of, as its build set it; "dev" for a build that isn't one. */
  release: string
  deployment: Deployment
  /** Opens the page again; location.reload() by default. */
  reload?: () => void
  /** The page's service worker registration, when it has one. */
  registration?: () => Promise<ServiceWorkerRegistration | undefined>
  /**
   * The release a worker keeps, which opening the page again would load, or
   * null if it doesn't say. By default the worker is asked with a message,
   * `{type: 'release'}`, which it answers on the port it's sent with `{release}`.
   */
  keptRelease?: (worker: ServiceWorker | null) => Promise<string | null>
  /** Where the last reload is noted, so a page that opens just as old waits; sessionStorage by default. */
  storage?: Pick<Storage, 'getItem' | 'setItem'>
  /** The capabilities endpoint's path, whose answers never make the flock be read again. */
  capabilitiesPath?: string
  retryAfter?: number
  reloadAfter?: number
  recheckAfter?: number
  timers?: Timers
  now?: () => number
}

export type AppUpdate = {
  state(): AppUpdateState
  subscribe(listener: () => void): () => void
  /** Tries again now to load the current release, after it couldn't be. */
  tryAgain(): void
  /** Hears the oldest page served, and the connection coming back; returns how to stop. */
  watch(target?: Pick<Window, 'addEventListener' | 'removeEventListener'>): () => void
  /**
   * Keeps the page's requests to what the flock serves it: none once it's out
   * of date; changes only once the flock has said, so a page opened from an
   * older release never sends one first; and an answer that could mean the
   * server no longer has what it asked for reads the flock again. Its shape is
   * openapi-fetch's middleware: `client.use(update.middleware)`.
   */
  middleware: {
    onRequest(context: { request: Request }): Promise<undefined>
    onResponse(context: { request: Request; response: Response }): undefined
  }
  /** The same, around a fetch function. */
  fetch(fetcher: typeof fetch): typeof fetch
}

/** Asks a worker which release it keeps. */
function askWorker(worker: ServiceWorker | null): Promise<string | null> {
  if (!worker) return Promise.resolve(null)
  return new Promise((resolve) => {
    const channel = new MessageChannel()
    const timer = setTimeout(() => resolve(null), 3000)
    channel.port1.onmessage = (event: MessageEvent<{ release?: string }>) => {
      clearTimeout(timer)
      resolve(event.data?.release ?? null)
    }
    worker.postMessage({ type: 'release' }, [channel.port2])
  })
}

/** Waits until an installing worker has taken over, or given up. */
function settled(worker: ServiceWorker | null | undefined) {
  if (!worker) return Promise.resolve()
  return new Promise<void>((resolve) => {
    const check = () => {
      if (worker.state === 'activated' || worker.state === 'redundant') resolve()
    }
    worker.addEventListener('statechange', check)
    check()
  })
}

export function createAppUpdate(options: AppUpdateOptions): AppUpdate {
  const { release, deployment } = options
  const timers: Timers = options.timers ?? { setTimeout: (callback, ms) => globalThis.setTimeout(callback, ms), clearTimeout: (handle) => globalThis.clearTimeout(handle as number) }
  const now = options.now ?? Date.now
  const retryAfter = options.retryAfter ?? 30_000
  // Opening again is tried at most this often: a page that opens just as old
  // (say, from a cache between it and the server) waits instead of reloading on.
  const reloadAfter = options.reloadAfter ?? 30_000
  const recheckAfter = options.recheckAfter ?? 30_000
  const capabilitiesPath = options.capabilitiesPath ?? '/api/capabilities'
  const doReload = options.reload ?? (() => globalThis.location.reload())
  const registration = options.registration ?? (async () => globalThis.navigator?.serviceWorker?.getRegistration().catch(() => undefined))
  const keptRelease = options.keptRelease ?? askWorker
  const storage = options.storage ?? (() => { try { return globalThis.sessionStorage } catch { return undefined } })()
  const reloadedKey = 'geese-update-reloaded'

  let state: AppUpdateState = 'current'
  let retry: unknown
  let lastRecheck = 0
  let lastReload = 0
  const listeners = new Set<() => void>()
  const setState = (next: AppUpdateState) => {
    if (state === next) return
    state = next
    for (const listener of listeners) listener()
  }

  /** Opens the page again, unless it did just now; says whether it did. */
  const reload = () => {
    let last = lastReload
    try { last = Math.max(last, Number(storage?.getItem(reloadedKey) ?? 0)) } catch { /* The timer below spaces the tries out. */ }
    if (now() - last < reloadAfter) return false
    lastReload = now()
    try { storage?.setItem(reloadedKey, String(lastReload)) } catch { /* As above. */ }
    doReload()
    return true
  }

  const stuck = () => {
    setState('stuck')
    timers.clearTimeout(retry)
    retry = timers.setTimeout(() => void moveOn(), retryAfter)
  }

  // Moving on: the service worker is asked for a newer release, and the page
  // opens again once it keeps one. Without a worker, opening again asks the
  // server. If none is to be had yet (a rollout's files aren't all there, or the
  // connection dropped), it's tried again shortly, and as the connection returns.
  const moveOn = async () => {
    setState('updating')
    const worker = await registration()
    if (!worker) {
      if (!reload()) stuck()
      return
    }
    try {
      await worker.update()
      await settled(worker.installing)
    } catch {
      // Not reachable, or its install failed: tried again below.
    }
    // A worker that keeps another release opens that one; one that keeps none
    // (or doesn't say) leaves opening to the server.
    const kept = await keptRelease(worker.active)
    if (kept !== release && reload()) return
    stuck()
  }

  const heardOldestPage = (oldestPage: string | undefined) => {
    if (state === 'current' && belowOldestPage(release, oldestPage)) void moveOn()
  }

  const beforeRequest = async (method: string) => {
    if (state !== 'current') throw new OutdatedPageError()
    if (method !== 'GET' && method !== 'HEAD') await deployment.whenRead()
    if (state !== 'current') throw new OutdatedPageError()
  }
  const afterResponse = (url: string, status: number) => {
    if (new URL(url, 'http://page').pathname === capabilitiesPath || !unexpectedAnswer(status)) return
    if (now() - lastRecheck > recheckAfter) {
      lastRecheck = now()
      void deployment.read()
    }
  }

  return {
    state: () => state,
    subscribe(listener) {
      listeners.add(listener)
      return () => { listeners.delete(listener) }
    },
    tryAgain() {
      if (state === 'stuck') void moveOn()
    },
    watch(target = globalThis.window) {
      const stopHearing = deployment.onOldestPage(heardOldestPage)
      // A floor already read is acted on now.
      heardOldestPage(deployment.oldestPage())
      const online = () => { if (state === 'stuck') void moveOn() }
      target.addEventListener('online', online)
      return () => {
        stopHearing()
        target.removeEventListener('online', online)
      }
    },
    middleware: {
      async onRequest({ request }) {
        await beforeRequest(request.method)
        return undefined
      },
      onResponse({ request, response }) {
        afterResponse(request.url, response.status)
        return undefined
      },
    },
    fetch(fetcher) {
      return async (input, init) => {
        const request = new Request(input, init)
        await beforeRequest(request.method)
        const response = await fetcher(request)
        afterResponse(request.url, response.status)
        return response
      }
    },
  }
}
