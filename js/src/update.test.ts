import assert from 'node:assert/strict'
import { describe, it } from 'node:test'

import { createDeployment, type DeploymentAnswer } from './deployment.ts'
import { createAppUpdate, OutdatedPageError } from './update.ts'

/** A flock that says what it's told to, and counts how often it's asked. */
function flock(answer: DeploymentAnswer | undefined) {
  const asked = { count: 0 }
  const deployment = createDeployment({ read: async () => { asked.count++; return answer } })
  return { deployment, asked, say: (next: DeploymentAnswer) => { answer = next } }
}

/** Timers the test runs by hand. */
function timers() {
  const pending: (() => void)[] = []
  return {
    timers: { setTimeout: (callback: () => void) => { pending.push(callback); return pending.length }, clearTimeout: () => { pending.length = 0 } },
    run: () => { const due = pending.splice(0); for (const callback of due) callback() },
  }
}

const tick = () => new Promise((resolve) => setTimeout(resolve, 0))
const request = (method: string, path = '/api/lists') => new Request(`http://page${path}`, { method })

describe('a page the flock still serves', () => {
  it('reads before it changes anything, and sends what it asks', async () => {
    const { deployment, asked } = flock({ capabilities: ['lists'], oldestPage: '7.0.0' })
    const update = createAppUpdate({ release: '7.2.0', deployment, reload: () => assert.fail('reloaded') })
    update.watch({ addEventListener() {}, removeEventListener() {} })
    await update.middleware.onRequest({ request: request('GET') })
    assert.equal(asked.count, 0, 'a read waits for nothing')
    await update.middleware.onRequest({ request: request('POST') })
    assert.equal(asked.count, 1, 'a change waits for the flock to be read')
    assert.equal(update.state(), 'current')
    assert.equal(deployment.has('lists'), true)
  })

  it('reads the flock again after an answer it didn’t expect, at most every so often', async () => {
    const { deployment, asked } = flock({ capabilities: [] })
    let now = 100_000
    const update = createAppUpdate({ release: '7.2.0', deployment, now: () => now })
    update.middleware.onResponse({ request: request('GET'), response: new Response(null, { status: 404 }) })
    update.middleware.onResponse({ request: request('GET'), response: new Response(null, { status: 404 }) })
    await tick()
    assert.equal(asked.count, 1)
    update.middleware.onResponse({ request: request('GET', '/api/capabilities'), response: new Response(null, { status: 404 }) })
    now += 31_000
    update.middleware.onResponse({ request: request('GET'), response: new Response(null, { status: 200 }) })
    await tick()
    assert.equal(asked.count, 1, 'an expected answer, or the capabilities’ own, reads nothing')
  })
})

describe('a page older than the flock serves', () => {
  it('stops calling the API and loads the current release', async () => {
    const { deployment } = flock({ capabilities: [], oldestPage: '8.0.0' })
    let reloaded = 0
    const update = createAppUpdate({ release: '7.2.0', deployment, reload: () => reloaded++, registration: async () => undefined, storage: new Map() as unknown as Storage })
    update.watch({ addEventListener() {}, removeEventListener() {} })
    await deployment.whenRead()
    await tick()
    assert.equal(reloaded, 1)
    assert.notEqual(update.state(), 'current')
    await assert.rejects(update.middleware.onRequest({ request: request('GET') }), OutdatedPageError)
    await assert.rejects(update.fetch(async () => new Response())('http://page/api/lists'), OutdatedPageError)
  })

  it('waits, and tries again, when the current release can’t be loaded yet', async () => {
    const { deployment } = flock({ capabilities: [], oldestPage: '8.0.0' })
    const clock = timers()
    let now = 1_000_000
    let reloaded = 0
    const storage = new Map<string, string>()
    const update = createAppUpdate({
      release: '7.2.0', deployment, now: () => now, timers: clock.timers,
      reload: () => reloaded++,
      storage: { getItem: (key) => storage.get(key) ?? null, setItem: (key, value) => { storage.set(key, value) } },
      // A worker that still keeps this release: opening again would load it.
      registration: async () => ({ update: async () => {}, installing: null, active: null }) as unknown as ServiceWorkerRegistration,
      keptRelease: async () => '7.2.0',
    })
    const states: string[] = []
    update.subscribe(() => states.push(update.state()))
    update.watch({ addEventListener() {}, removeEventListener() {} })
    await deployment.whenRead()
    await tick()
    assert.equal(update.state(), 'stuck')
    assert.equal(reloaded, 0)
    // Its rollout's files arrive: the worker keeps the new release, and the page opens it.
    update.tryAgain()
    await tick()
    assert.equal(update.state(), 'stuck', 'the same release again opens nothing')
    const kept = createAppUpdate({
      release: '7.2.0', deployment, now: () => now, timers: clock.timers, reload: () => reloaded++,
      storage: { getItem: (key) => storage.get(key) ?? null, setItem: (key, value) => { storage.set(key, value) } },
      registration: async () => ({ update: async () => {}, installing: null, active: null }) as unknown as ServiceWorkerRegistration,
      keptRelease: async () => '8.0.0',
    })
    kept.watch({ addEventListener() {}, removeEventListener() {} })
    await tick()
    assert.equal(reloaded, 1)
    // The page that opens is just as old (a cache between it and the server): it waits instead of reloading on.
    now += 1000
    const again = createAppUpdate({
      release: '7.2.0', deployment, now: () => now, timers: clock.timers, reload: () => reloaded++,
      storage: { getItem: (key) => storage.get(key) ?? null, setItem: (key, value) => { storage.set(key, value) } },
      registration: async () => undefined,
    })
    again.watch({ addEventListener() {}, removeEventListener() {} })
    await tick()
    assert.equal(reloaded, 1)
    assert.equal(again.state(), 'stuck')
    assert.deepEqual(states.slice(0, 2), ['updating', 'stuck'])
  })

  it('never leaves a build that isn’t a release', async () => {
    const { deployment } = flock({ capabilities: [], oldestPage: '8.0.0' })
    const update = createAppUpdate({ release: 'dev', deployment, reload: () => assert.fail('reloaded') })
    update.watch({ addEventListener() {}, removeEventListener() {} })
    await deployment.whenRead()
    await tick()
    assert.equal(update.state(), 'current')
  })
})
