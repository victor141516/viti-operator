import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'

const wrapperSource = readFileSync(
  new URL('../android/app/src/main/assets/android-voice.js', import.meta.url),
  'utf8',
)

function createHarness() {
  const calls = []
  const instances = []
  const context = {
    document: { querySelector: () => ({}) },
    VitiAndroid: { onVoiceEvent: (payload) => calls.push(['event', JSON.parse(payload)]) },
    VitiVoiceBundle: {
      createVoice: () => {
        const voice = {
          start: (value) => calls.push(['start', value]),
          stop: () => calls.push(['stop']),
          call: (...args) => { calls.push(['call', ...args]); return args[2] },
          answer: (...args) => { calls.push(['answer', ...args]); return true },
          reject: (...args) => { calls.push(['reject', ...args]); return true },
          hangup: (...args) => { calls.push(['hangup', ...args]); return true },
		  reconnect: (...args) => { calls.push(['reconnect', ...args]); return args[1] },
          mute: (...args) => { calls.push(['mute', ...args]); return args[0] },
          setTelecomMuted: (...args) => { calls.push(['telecom-muted', ...args]); return true },
          setSystemInactive: (...args) => { calls.push(['inactive', ...args]); return true },
          dtmf: (...args) => { calls.push(['dtmf', ...args]); return true },
        }
        instances.push(voice)
        return voice
      },
    },
  }
  context.window = context
  vm.runInNewContext(wrapperSource, context)
  return { context, calls, instances }
}

test('Android commands preserve the immutable call-leg token', () => {
  const { context, calls } = createHarness()
  context.vitiVoiceStart(JSON.stringify({ enabled: true, websocket_url: 'wss://example.invalid' }), '{}')

  const callAck = JSON.parse(context.vitiVoiceCommand(JSON.stringify({
    action: 'call',
    leg_token: 'generation-42',
    args: { number: '+34600000000', trace_id: 'trace-1' },
  })))
  const answerAck = JSON.parse(context.vitiVoiceCommand(JSON.stringify({
    action: 'answer',
    leg_token: 'generation-42',
    args: {},
  })))

  assert.deepEqual(callAck, { ok: true, leg_token: 'generation-42', result: 'generation-42' })
  assert.equal(answerAck.ok, true)
  assert.deepEqual(calls.find(([kind]) => kind === 'call'), [
    'call', '+34600000000', 'trace-1', 'generation-42',
  ])
  assert.deepEqual(calls.find(([kind]) => kind === 'answer'), ['answer', 'generation-42'])
})

test('disabling provisioning stops the previous engine without creating a blank UA', () => {
  const { context, calls, instances } = createHarness()
  context.vitiVoiceStart(JSON.stringify({ enabled: true, websocket_url: 'wss://example.invalid' }), '{}')
  context.vitiVoiceStart(JSON.stringify({ enabled: false }), '{}')

  assert.equal(instances.length, 1)
  assert.equal(calls.filter(([kind]) => kind === 'stop').length, 1)
  assert.equal(calls.filter(([kind]) => kind === 'start').length, 1)
  assert.equal(calls.at(-1)[0], 'event')
  assert.equal(calls.at(-1)[1].type, 'disconnected')
})

test('Android reconnect keeps the logical leg and carrier trace identity', () => {
	const { context, calls } = createHarness()
	context.vitiVoiceStart(JSON.stringify({ enabled: true, websocket_url: 'wss://example.invalid' }), '{}')
	const ack = JSON.parse(context.vitiVoiceCommand(JSON.stringify({
		action: 'reconnect', leg_token: 'logical-leg-42', args: { trace_id: 'carrier-trace-7' },
	})))
	assert.equal(ack.ok, true)
	assert.deepEqual(calls.find(([kind]) => kind === 'reconnect'), ['reconnect', 'carrier-trace-7', 'logical-leg-42'])
})
