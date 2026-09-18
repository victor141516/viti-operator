import test from 'node:test'
import assert from 'node:assert/strict'

import { incomingPeerName, isDefinitiveRemoteSessionEnd } from './voice.js'

test('a remote SIP BYE is a definitive call end', () => {
  assert.equal(isDefinitiveRemoteSessionEnd({ originator: 'remote', cause: 'Terminated' }), true)
})

test('local and transport terminations remain eligible for media recovery', () => {
  assert.equal(isDefinitiveRemoteSessionEnd({ originator: 'local', cause: 'Terminated' }), false)
  assert.equal(isDefinitiveRemoteSessionEnd({ originator: 'system', cause: 'Connection Error' }), false)
  assert.equal(isDefinitiveRemoteSessionEnd({ cause: 'WebRTC Error' }), false)
})

test('incoming calls prefer the SIP display name', () => {
  assert.equal(incomingPeerName({ display_name: 'Viti Server down', uri: { user: 'viti-server-down' } }), 'Viti Server down')
  assert.equal(incomingPeerName({ uri: { user: '+34123456789' } }), '+34123456789')
  assert.equal(incomingPeerName(), 'Private number')
})
