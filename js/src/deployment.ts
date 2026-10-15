// What the flock says to pages: the capabilities it has switched on, and the
// oldest release whose pages it serves. Read when the page opens, and again as
// it comes back into view.

/** What the flock says, as an API's capabilities endpoint answers. */
export type DeploymentAnswer = { capabilities: string[]; oldestPage?: string }

export type DeploymentOptions = {
  /**
   * Reads what the flock says, such as `GET /api/capabilities`. It may reject,
   * or resolve undefined, when it can't (offline): what was known stays.
   */
  read: () => Promise<DeploymentAnswer | undefined>
  /** How long after a read coming back into view reads again; a minute by default. */
  rereadAfter?: number
  now?: () => number
}

export type Deployment = {
  /** Reads what the flock says again; settles once read, or not. Reads at once coalesce. */
  read(): Promise<void>
  /** Settles once the flock has been read for the first time, however that went. */
  whenRead(): Promise<void>
  /** The capabilities switched on, or null until known. */
  active(): readonly string[] | null
  /** Whether a capability is on; false until known. */
  has(name: string): boolean
  /** The oldest release whose pages the flock serves, as "8.0.0", if it has said. */
  oldestPage(): string | undefined
  /** Hears every read that changes what's known, for a store such as React's useSyncExternalStore. */
  subscribe(listener: () => void): () => void
  /** Increases with every change, for useSyncExternalStore's snapshot. */
  version(): number
  /** Hears the oldest page served, each time it's read. */
  onOldestPage(listener: (oldestPage: string | undefined) => void): () => void
  /** Reads as the page opens, and again as it comes back into view; returns how to stop. */
  watch(target?: Pick<Window, 'addEventListener' | 'removeEventListener'> & { document: Pick<Document, 'visibilityState' | 'addEventListener' | 'removeEventListener'> }): () => void
}

export function createDeployment(options: DeploymentOptions): Deployment {
  const now = options.now ?? Date.now
  const rereadAfter = options.rereadAfter ?? 60_000
  let active: string[] | null = null
  let oldest: string | undefined
  let version = 0
  let loading: Promise<void> | null = null
  let firstRead: Promise<void> | null = null
  let lastRead = 0
  const listeners = new Set<() => void>()
  const oldestListeners = new Set<(oldestPage: string | undefined) => void>()

  const read = () => {
    lastRead = now()
    loading ??= options.read()
      .then((answer) => {
        if (!answer) return
        active = [...answer.capabilities]
        oldest = answer.oldestPage
        version++
        for (const listener of listeners) listener()
        for (const listener of oldestListeners) listener(answer.oldestPage)
      })
      .catch(() => { /* Offline, or this page no longer asks: as it was. */ })
      .finally(() => { loading = null })
    return loading
  }
  const whenRead = () => {
    firstRead ??= read()
    return firstRead
  }

  return {
    read,
    whenRead,
    active: () => active,
    has: (name) => !!active?.includes(name),
    oldestPage: () => oldest,
    subscribe(listener) {
      listeners.add(listener)
      if (active === null) void whenRead()
      return () => { listeners.delete(listener) }
    },
    version: () => version,
    onOldestPage(listener) {
      oldestListeners.add(listener)
      return () => { oldestListeners.delete(listener) }
    },
    watch(target = globalThis.window) {
      void whenRead()
      const reread = () => {
        if (target.document.visibilityState === 'visible' && now() - lastRead > rereadAfter) void read()
      }
      target.addEventListener('focus', reread)
      target.document.addEventListener('visibilitychange', reread)
      return () => {
        target.removeEventListener('focus', reread)
        target.document.removeEventListener('visibilitychange', reread)
      }
    },
  }
}
