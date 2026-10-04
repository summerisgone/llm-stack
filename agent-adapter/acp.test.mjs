import assert from 'node:assert/strict'
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'
import { startACP } from './acp.mjs'

// A minimal ACP agent: advertises CAPS, logs every request method to LOG, and
// answers a prompt with one tool call that asks for permission.
const AGENT = `
const fs = require('fs')
const rl = require('readline').createInterface({ input: process.stdin })
const send = (m) => process.stdout.write(JSON.stringify({ jsonrpc: '2.0', ...m }) + '\\n')
const waiting = new Map()
let n = 1000
rl.on('line', async (line) => {
  const m = JSON.parse(line)
  if (m.method === undefined) return waiting.get(m.id)?.(m.result)
  fs.appendFileSync(process.env.LOG, m.method + '\\n')
  if (m.method === 'initialize') return send({ id: m.id, result: { protocolVersion: 1, agentCapabilities: JSON.parse(process.env.CAPS) } })
  if (m.method === 'session/new') return send({ id: m.id, result: { sessionId: 'new-' + Date.now() } })
  if (m.method === 'session/resume' || m.method === 'session/load') {
    return m.params.sessionId === 'known' ? send({ id: m.id, result: {} }) : send({ id: m.id, error: { code: -32602, message: 'unknown' } })
  }
  if (m.method === 'session/prompt') {
    const sid = m.params.sessionId
    const up = (u) => send({ method: 'session/update', params: { sessionId: sid, update: u } })
    up({ sessionUpdate: 'tool_call', toolCallId: 't', title: 'ls', kind: 'execute', status: 'pending' })
    const id = ++n
    const r = await new Promise((res) => { waiting.set(id, res); send({ id, method: 'session/request_permission', params: { sessionId: sid, toolCall: { toolCallId: 't', title: 'ls' },
      options: [{ optionId: 'y', kind: 'allow_once' }, { optionId: 'no', kind: 'reject_once' }] } }) })
    up({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: JSON.stringify(r.outcome) } })
    send({ id: m.id, result: { stopReason: 'end_turn' } })
  }
})
`

async function agent(caps, map = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'acp-'))
  writeFileSync(join(dir, 'agent.cjs'), AGENT)
  writeFileSync(join(dir, 'map.json'), JSON.stringify(map))
  const log = join(dir, 'log')
  writeFileSync(log, '')
  const backend = await startACP({
    name: 'fake', command: process.execPath, args: [join(dir, 'agent.cjs')], cwd: dir,
    mapPath: join(dir, 'map.json'), env: { ...process.env, LOG: log, CAPS: JSON.stringify(caps) },
  })
  return { backend, methods: () => readFileSync(log, 'utf8').trim().split('\n'), map: () => JSON.parse(readFileSync(join(dir, 'map.json'), 'utf8')) }
}

const turn = (backend, key, opts = {}) => {
  let text = ''
  return backend.prompt(key, 'hi', { onText: (t) => { text += t }, onReasoning() {}, signal: new AbortController().signal, ...opts }).then(() => text)
}

test('reattaches with the advertised method, then creates', async () => {
  const a = await agent({ loadSession: true }, { c1: 'known', c2: 'gone' })
  await turn(a.backend, 'c1')
  await turn(a.backend, 'c2')
  assert.deepEqual(a.methods(), ['initialize', 'session/load', 'session/prompt', 'session/load', 'session/new', 'session/prompt'])
  assert.notEqual(a.map().c2, 'gone')
  assert.ok(a.backend.hasSession('c1') && !a.backend.hasSession('c3'))
  await a.backend.stop()
})

test('resume first when advertised, resume when nothing is', async () => {
  const a = await agent({ loadSession: true, sessionCapabilities: { resume: {} } }, { c1: 'known' })
  await turn(a.backend, 'c1')
  assert.deepEqual(a.methods().slice(1, 2), ['session/resume'])
  await a.backend.stop()
  const b = await agent({}, { c1: 'known' })
  await turn(b.backend, 'c1')
  assert.deepEqual(b.methods().slice(1, 2), ['session/resume'])
  await b.backend.stop()
})

test('permission: the user decides on the interaction path, approved otherwise', async () => {
  const a = await agent({})
  const tools = []
  assert.equal(await turn(a.backend, 'k', { onTool: (t) => tools.push(t), onPermission: async () => 'deny' }),
    JSON.stringify({ outcome: 'selected', optionId: 'no' }))
  assert.equal(tools[0].title, 'ls')
  assert.equal(await turn(a.backend, 'k', { onPermission: async () => 'cancel' }), JSON.stringify({ outcome: 'cancelled' }))
  assert.equal(await turn(a.backend, 'k'), JSON.stringify({ outcome: 'selected', optionId: 'y' }))
  await a.backend.stop()
})
