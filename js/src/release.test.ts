import assert from 'node:assert/strict'
import { describe, it } from 'node:test'

import { belowOldestPage, releaseMajor, unexpectedAnswer } from './release.ts'

describe('a page’s release', () => {
  it('is its major version', () => {
    assert.equal(releaseMajor('8.1.0'), 8)
    assert.equal(releaseMajor('8.1.0+0a1b2c3d4e5f'), 8)
    assert.equal(releaseMajor('v12.0.3'), 12)
  })

  it('is none for a build that isn’t a release', () => {
    for (const build of ['dev', '', '0.0.0-dev.0a1b2c3d4e5f', '8.1.0-rc.1', '8.1']) assert.equal(releaseMajor(build), null, build)
  })
})

describe('the oldest page the flock serves', () => {
  it('leaves out pages of an older major', () => {
    assert.equal(belowOldestPage('7.9.3', '8.0.0'), true)
    assert.equal(belowOldestPage('7.9.3+0a1b2c3d4e5f', '8.0.0'), true)
  })

  it('keeps pages of its major and later', () => {
    assert.equal(belowOldestPage('8.0.0', '8.0.0'), false)
    assert.equal(belowOldestPage('8.4.1', '8.0.0'), false)
    assert.equal(belowOldestPage('9.0.0', '8.0.0'), false)
  })

  it('keeps every page until the flock says, and a build that isn’t a release always', () => {
    assert.equal(belowOldestPage('7.0.0', undefined), false)
    assert.equal(belowOldestPage('7.0.0', null), false)
    assert.equal(belowOldestPage('dev', '8.0.0'), false)
  })
})

describe('an answer the page didn’t expect', () => {
  it('is a request or a path the server doesn’t have', () => {
    for (const status of [400, 404, 405, 410]) assert.equal(unexpectedAnswer(status), true, String(status))
    for (const status of [200, 204, 401, 403, 409, 429, 500, 503]) assert.equal(unexpectedAnswer(status), false, String(status))
  })
})
