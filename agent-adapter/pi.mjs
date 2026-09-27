// pi backend: the pi SDK in-process. hermes-sync has written the agent dir
// <home>/pi (settings.json, models.json, mcp.json, AGENTS.md); pi-mcp-adapter
// from this image is loaded as an extension because pi has no MCP client.
// Each chat is a persistent pi session under <home>/pi-sessions/<key>/.
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

// Installed next to the adapter in the pi image (Dockerfile).
const MCP_EXTENSION = fileURLToPath(new URL('./node_modules/pi-mcp-adapter', import.meta.url))

export async function start({ home, cwd }) {
  const agentDir = join(home, 'pi')
  // Read by pi and by pi-mcp-adapter (mcp.json location) through getAgentDir().
  process.env.PI_CODING_AGENT_DIR = agentDir
  const { createAgentSession, DefaultResourceLoader, SessionManager } = await import('@earendil-works/pi-coding-agent')
  const sessions = new Map()

  async function session(key) {
    if (sessions.has(key)) return sessions.get(key)
    const resourceLoader = new DefaultResourceLoader({ cwd, agentDir, additionalExtensionPaths: [MCP_EXTENSION] })
    await resourceLoader.reload()
    const { session } = await createAgentSession({
      cwd, agentDir, resourceLoader,
      sessionManager: SessionManager.continueRecent(cwd, join(home, 'pi-sessions', key)),
    })
    sessions.set(key, session)
    return session
  }

  return {
    async prompt(key, text, { onText, onReasoning, signal }) {
      const s = await session(key)
      const unsubscribe = s.subscribe((event) => {
        if (event.type !== 'message_update') return
        const e = event.assistantMessageEvent
        if (e.type === 'text_delta') onText(e.delta)
        else if (e.type === 'thinking_delta') onReasoning(e.delta)
      })
      const onAbort = () => s.abort()
      signal.addEventListener('abort', onAbort)
      try {
        await s.prompt(text)
      } finally {
        signal.removeEventListener('abort', onAbort)
        unsubscribe()
      }
      const last = s.messages.at(-1)
      if (last?.role === 'assistant' && last.stopReason === 'error') throw new Error(last.errorMessage || 'model error')
      return s.getLastAssistantText() || ''
    },
    async stop() {
      for (const s of sessions.values()) s.dispose()
    },
  }
}
