import assert from 'node:assert/strict'
import { test } from 'node:test'

import { fuzzyScore, rank } from './fuzzy.ts'

test('matches a subsequence and rejects a non-match', () => {
  assert.notEqual(fuzzyScore('svc', 'service_backend'), null)
  assert.equal(fuzzyScore('zzz', 'service_backend'), null)
  // Order matters: the characters must appear in sequence.
  assert.equal(fuzzyScore('cvs', 'svc_v1'), null)
})

test('is case insensitive', () => {
  assert.notEqual(fuzzyScore('SVC', 'svc_v1'), null)
  assert.notEqual(fuzzyScore('svc', 'SVC_V1'), null)
})

test('an empty query matches everything', () => {
  assert.equal(fuzzyScore('', 'anything'), 0)
})

test('prefers a prefix over a match in the middle', () => {
  const prefix = fuzzyScore('svc', 'svc_v1')!
  const middle = fuzzyScore('svc', 'my_svc')!
  assert.ok(prefix > middle, `prefix ${prefix} should beat middle ${middle}`)
})

// The point of the separator bonus: a query made of segment initials should
// find the structured name, not an incidental scattering of the same letters.
test('rewards matches at segment boundaries', () => {
  const boundary = fuzzyScore('osc', 'outbound|8080|svc|cluster')!
  const scattered = fuzzyScore('osc', 'oxxsxxcxx')!
  assert.ok(boundary > scattered, `boundary ${boundary} should beat ${scattered}`)
})

test('rewards consecutive characters', () => {
  const together = fuzzyScore('back', 'xback')!
  const apart = fuzzyScore('back', 'bxaxcxk')!
  assert.ok(together > apart, `consecutive ${together} should beat ${apart}`)
})

test('prefers the shorter of two equally good matches', () => {
  const short = fuzzyScore('svc', 'svc')!
  const long = fuzzyScore('svc', 'svc_with_a_very_long_tail')!
  assert.ok(short > long)
})

test('ranks the obvious candidate first', () => {
  const names = [
    'ext_authz_cluster',
    'svc_v1',
    'outbound|8080|v1|svc.ns.svc.cluster.local',
    'tcp_backend',
  ]
  const results = rank('svcv1', names, (n) => n)
  assert.equal(results[0].item, 'svc_v1')
})

test('honours the limit and drops non-matches', () => {
  const names = ['aaa', 'aab', 'aac', 'zzz']
  const results = rank('aa', names, (n) => n, 2)
  assert.equal(results.length, 2)
  assert.ok(results.every((r) => r.item.startsWith('aa')))
})
