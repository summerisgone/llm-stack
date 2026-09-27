import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createServer, lastUserText, sessionKey } from './server.mjs'

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
