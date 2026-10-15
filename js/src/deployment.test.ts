import assert from 'node:assert/strict'
import { describe, it } from 'node:test'

import { createDeployment } from './deployment.ts'

/** A window the test drives. */
function page() {
  const listeners = new Map<string, () => void>()
  const doc = { visibilityState: 'visible' as DocumentVisibilityState, addEventListener: (type: string, listener: () => void) => listeners.set(`doc:${type}`, listener), removeEventListener: (type: string) => listeners.delete(`doc:${type}`) }
  return {
    target: { document: doc, addEventListener: (type: string, listener: () => void) => listeners.set(type, listener), removeEventListener: (type: string) => listeners.delete(type) } as never,
    fire: (type: string) => listeners.get(type)?.(),
    doc,
    listeners,
  }
}

describe('what the flock says', () => {
  it('is read once as the page opens, and again as it comes back into view after a while', async () => {
    let asked = 0
    let now = 0
    const deployment = createDeployment({ read: async () => { asked++; return { capabilities: ['lists'], oldestPage: '1.0.0' } }, now: () => now })
    const window = page()
    const stop = deployment.watch(window.target)
    await deployment.whenRead()
    assert.equal(asked, 1)
    assert.deepEqual(deployment.active(), ['lists'])
    assert.equal(deployment.oldestPage(), '1.0.0')
    window.fire('focus')
    assert.equal(asked, 1, 'not again within a minute')
    now = 61_000
    window.doc.visibilityState = 'hidden'
    window.fire('doc:visibilitychange')
    assert.equal(asked, 1, 'not while hidden')
    window.doc.visibilityState = 'visible'
    window.fire('focus')
    await deployment.read()
    assert.equal(asked, 2)
    stop()
    assert.equal(window.listeners.size, 0)
  })

  it('stays as it was when it can’t be read', async () => {
    const answers = [{ capabilities: ['lists'] }, undefined]
    const deployment = createDeployment({ read: async () => { const answer = answers.shift(); if (answer === undefined && answers.length === 0) throw new Error('offline'); return answer } })
    let heard = 0
    deployment.subscribe(() => heard++)
    await deployment.whenRead()
    await deployment.read()
    assert.equal(deployment.has('lists'), true)
    assert.equal(heard, 1)
    assert.equal(deployment.has('comments'), false)
  })

  it('reads once for reads at once', async () => {
    let asked = 0
    const deployment = createDeployment({ read: async () => { asked++; return { capabilities: [] } } })
    await Promise.all([deployment.read(), deployment.read(), deployment.whenRead()])
    assert.equal(asked, 1)
  })
})
