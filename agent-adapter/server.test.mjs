import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { test } from 'node:test'
import { chatKey, continues, createServer, lastUserText, sessionKey } from './server.mjs'

function listen(backend) {
  const server = createServer({ backend, apiKey: 'k', model: 'pi-agent' })
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve(server)))
}

const post = (server, body, headers = {}) => fetch(`http://127.0.0.1:${server.address().port}/v1/chat/completions`, {
  method: 'POST',
  headers: { Authorization: 'Bearer k', 'Content-Type': 'application/json', ...headers },
  body: JSON.stringify(body),
})

test('last user text and session key', () => {
  const messages = [{ role: 'user', content: 'first' }, { role: 'assistant', content: 'a' },
    { role: 'user', content: [{ type: 'text', text: 'sec' }, { type: 'image_url' }, { type: 'text', text: 'ond' }] }]
  assert.equal(lastUserText(messages), 'second')
  assert.equal(sessionKey({ 'x-openwebui-chat-id': 'chat-1' }, messages), 'chat-1')
  assert.equal(sessionKey({ 'x-openwebui-chat-id': '../x' }, messages), sessionKey({}, messages.slice(0, 1)))
})

test('rejects a wrong key', async () => {
  const server = await listen({})
  const res = await post(server, { messages: [] }, { Authorization: 'Bearer nope' })
  assert.equal(res.status, 401)
  server.close()
})

test('streams deltas and sends only the last user message per chat', async () => {
  const calls = []
  const server = await listen({
    async prompt(key, text, { onText, onReasoning }) {
      calls.push([key, text])
      onReasoning('hmm')
      onText('hel')
      onText('lo')
      return 'hello'
    },
  })
  const res = await post(server, { stream: true, messages: [{ role: 'user', content: 'a' }, { role: 'assistant', content: 'x' }, { role: 'user', content: 'b' }] },
    { 'X-OpenWebUI-Chat-Id': 'c1' })
  const body = await res.text()
  assert.deepEqual(calls, [['c1', 'b']])
  const deltas = body.split('\n\n').filter((l) => l.startsWith('data: {')).map((l) => JSON.parse(l.slice(6)).choices[0])
  assert.equal(deltas.map((d) => d.delta.content || '').join(''), 'hello')
  assert.equal(deltas.find((d) => d.delta.reasoning_content).delta.reasoning_content, 'hmm')
  assert.equal(deltas.at(-1).finish_reason, 'stop')
  assert.ok(body.endsWith('data: [DONE]\n\n'))

  const plain = await (await post(server, { messages: [{ role: 'user', content: 'q' }] }, { 'X-OpenWebUI-Chat-Id': 'c1' })).json()
  assert.equal(plain.choices[0].message.content, 'hello')
  server.close()
})

test('one turn at a time per session', async () => {
  let running = 0
  let max = 0
  const server = await listen({
    async prompt() {
      max = Math.max(max, ++running)
      await new Promise((r) => setTimeout(r, 30))
      running--
      return 'ok'
    },
  })
  const body = { messages: [{ role: 'user', content: 'x' }] }
  await Promise.all([post(server, body, { 'X-OpenWebUI-Chat-Id': 's' }), post(server, body, { 'X-OpenWebUI-Chat-Id': 's' })])
  assert.equal(max, 1)
  server.close()
})

test('backend error ends the stream with a message', async () => {
  const server = await listen({ async prompt() { throw new Error('boom') } })
  const body = await (await post(server, { stream: true, messages: [{ role: 'user', content: 'x' }] })).text()
  assert.match(body, /Agent error: boom/)
  const res = await post(server, { messages: [{ role: 'user', content: 'x' }] })
  assert.equal(res.status, 502)
  server.close()
})

test('web token only for a backend that has one', async () => {
  const get = (server, headers) => fetch(`http://127.0.0.1:${server.address().port}/web-token`, { headers })
  const server = await listen({ webToken: () => 't1' })
  assert.equal((await get(server, { Authorization: 'Bearer nope' })).status, 401)
  assert.deepEqual(await (await get(server, { Authorization: 'Bearer k' })).json(), { token: 't1' })
  server.close()
  const none = await listen({})
  assert.equal((await get(none, { Authorization: 'Bearer k' })).status, 404)
  none.close()
})

const api = (server, path, body) => fetch(`http://127.0.0.1:${server.address().port}${path}`, {
  method: 'POST',
  headers: { Authorization: 'Bearer k', 'Content-Type': 'application/json' },
  body: JSON.stringify(body),
})

// readEvents reads NDJSON events, calling onEvent for each as it arrives.
async function readEvents(res, onEvent = () => {}) {
  const events = []
  let buf = ''
  for await (const chunk of res.body) {
    buf += Buffer.from(chunk).toString('utf8')
    let i
    while ((i = buf.indexOf('\n')) >= 0) {
      const e = JSON.parse(buf.slice(0, i))
      buf = buf.slice(i + 1)
      events.push(e)
      await onEvent(e)
    }
  }
  return events
}

function listenInteractive(backend, opts = {}) {
  const server = createServer({ backend: { interactive: true, ...backend }, apiKey: 'k', model: 'dsh-agent', ...opts })
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve(server)))
}

test('history continuation', () => {
  assert.equal(chatKey('c1', ['a']), 'c1')
  assert.equal(chatKey('local:x', ['a']), chatKey(null, ['a']))
  const h = (t) => createHash('sha256').update(t).digest('hex').slice(0, 16)
  assert.ok(continues([], []))
  assert.ok(continues([h('a')], ['a']))
  assert.ok(continues([h('a')], ['/skills', 'a', 'failed']))
  assert.ok(!continues([h('a'), h('b')], ['a']))
  assert.ok(!continues([h('a'), h('b')], ['a', 'edited']))
})

test('interaction turn: events, permission approval and history', async () => {
  const seen = []
  const server = await listenInteractive({
    hasSession: () => false,
    async prompt(key, text, { onText, onTool, onPermission, onStop }) {
      onTool({ id: 't1', title: 'ls', status: 'pending', text: '' })
      seen.push(await onPermission({ title: 'ls', text: '' }))
      onText('done')
      onStop('end_turn')
      return 'done'
    },
  })
  const events = await readEvents(await api(server, '/v1/agent/turns', { chat_id: 'c', users: ['a'] }), async (e) => {
    if (e.type === 'permission') {
      assert.equal((await api(server, '/v1/agent/permissions', { turn: 'other', request: e.request, allow: true })).status, 404)
      assert.equal((await api(server, '/v1/agent/permissions', { turn: e.turn, request: e.request, allow: true })).status, 200)
      assert.equal((await api(server, '/v1/agent/permissions', { turn: e.turn, request: e.request, allow: false })).status, 404)
    }
  })
  assert.deepEqual(events.map((e) => e.type), ['turn', 'tool', 'permission', 'text', 'done'])
  assert.deepEqual(seen, ['allow'])
  assert.equal(events.at(-1).stop, 'end_turn')

  const next = await readEvents(await api(server, '/v1/agent/turns', { chat_id: 'c', users: ['a', 'b'] }), async (e) => {
    if (e.type === 'permission') await api(server, '/v1/agent/permissions', { turn: e.turn, request: e.request, allow: false })
  })
  assert.equal(next.at(-1).type, 'done')
  assert.deepEqual(seen, ['allow', 'deny'])

  const regen = await readEvents(await api(server, '/v1/agent/turns', { chat_id: 'c', users: ['a', 'b'] }))
  assert.equal(regen.at(-1).type, 'error')
  assert.match(regen.at(-1).message, /cannot edit, regenerate or branch/)
  server.close()
})

test('a chat the agent has no session for gets a notice', async () => {
  const server = await listenInteractive({ hasSession: () => false, async prompt() { return '' } })
  const events = await readEvents(await api(server, '/v1/agent/turns', { chat_id: 'old', users: ['earlier', 'now'] }))
  assert.deepEqual(events.map((e) => e.type), ['turn', 'notice', 'done'])
  server.close()
})

test('permission timeout denies', async () => {
  const decisions = []
  const server = await listenInteractive({
    async prompt(key, text, { onPermission }) {
      decisions.push(await onPermission({ title: 'rm', text: '' }))
      return ''
    },
  }, { permissionTimeout: 20 })
  const events = await readEvents(await api(server, '/v1/agent/turns', { chat_id: 'c1', users: ['x'] }))
  assert.equal(events.at(-1).type, 'done')
  assert.deepEqual(decisions, ['deny'])
  server.close()
})

test('disconnect cancels a pending permission request and the turn', async () => {
  let decided
  const decision = new Promise((r) => { decided = r })
  let aborted = false
  const server = await listenInteractive({
    async prompt(key, text, { onPermission, signal }) {
      signal.addEventListener('abort', () => { aborted = true })
      decided(await onPermission({ title: 'rm', text: '' }))
      return ''
    },
  })
  const ctl = new AbortController()
  const res = await fetch(`http://127.0.0.1:${server.address().port}/v1/agent/turns`, {
    method: 'POST', signal: ctl.signal,
    headers: { Authorization: 'Bearer k', 'Content-Type': 'application/json' },
    body: JSON.stringify({ chat_id: 'c2', users: ['x'] }),
  })
  await readEvents(res, (e) => e.type === 'permission' && ctl.abort()).catch(() => {})
  assert.equal(await decision, 'cancel')
  assert.ok(aborted)
  server.close()
})

test('non-interactive backends do not serve turns', async () => {
  const server = await listen({})
  assert.equal((await api(server, '/v1/agent/turns', { users: ['x'] })).status, 404)
  server.close()
})
