// Butler relay plugin — automatic message delivery into OpenCode sessions.
//
// butler-mcp (the MCP server OpenCode spawns) registers with the butler
// server and writes its identity + instance token to <workdir>/.butler/instance.json.
// This plugin reads that file, long-polls the butler server as the same agent
// identity, and relays every new message into the most recently active
// OpenCode session as a prompt — so agents receive butler messages without
// calling wait_for_messages/check_messages.
//
// Behavior:
//   - Messages sent by this identity itself (and "system" join/leave notices)
//     are skipped; chat and context updates are relayed.
//   - Delivery goes to the session that most recently ran a model request
//     (tracked via the "context" hook). Before any session is seen, or with
//     options.bridge = true, a dedicated "Butler" session is created and
//     reused instead.
//   - The cursor starts at the server's latest seq: messages older than this
//     OpenCode instance are not replayed (agents can read_session for history).
//   - On 401 (another instance of the same identity re-registered), the state
//     file is re-read and polling continues with the fresh token.
//
// Options (when loaded via the `plugins` array in opencode.json):
//   stateFile  — explicit path to the instance.json state file
//   bridge     — always deliver to the dedicated "Butler" session
//   delivery   — "queue" (default) or "steer"
//
// Known caveat: two OpenCode windows in the same directory share one butler
// identity; both plugins read the same (latest) token and may relay the same
// message into their own sessions.

import { Plugin } from "@opencode/plugin"
import { existsSync, readFileSync } from "node:fs"
import * as path from "node:path"

interface ButlerInstance {
  agent: string
  token: string
  server: string
}

interface ButlerMessage {
  seq: number
  session_id: string
  sender_id: string
  kind: string
  content: string
}

const WAIT_TIMEOUT_SECS = 25
const MAX_BACKOFF_MS = 60_000

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise(resolve => {
    const t = setTimeout(resolve, ms)
    signal?.addEventListener("abort", () => { clearTimeout(t); resolve() }, { once: true })
  })
}

export default Plugin.define({
  id: "butler",
  async setup(ctx) {
    const opts = ((ctx as any).options ?? {}) as {
      stateFile?: string
      bridge?: boolean
      delivery?: "queue" | "steer"
    }

    const candidates: string[] = [
      ...(opts.stateFile ? [opts.stateFile] : []),
      path.join(ctx.location.directory, ".butler", "instance.json"),
      ...(ctx.location.project?.canonical
        ? [path.join(ctx.location.project.canonical, ".butler", "instance.json")]
        : []),
    ]

    const readInstance = (): ButlerInstance | undefined => {
      for (const file of candidates) {
        try {
          if (!existsSync(file)) continue
          const raw = JSON.parse(readFileSync(file, "utf8"))
          if (
            raw &&
            typeof raw.agent === "string" &&
            typeof raw.token === "string" &&
            typeof raw.server === "string"
          ) {
            return raw as ButlerInstance
          }
        } catch (err) {
          // Partial write or invalid JSON — try the next candidate.
          console.error(`[butler] failed reading ${file}:`, err)
        }
      }
      return undefined
    }

    // Track the most recently active session (any model request in the agent
    // loop) so relays land in the conversation the agent is actually using.
    let lastActive: { sessionID: string, at: number } | undefined
    await ctx.session.hook("context", (event: any) => {
      const sid = event?.sessionID
      if (typeof sid === "string") lastActive = { sessionID: sid, at: Date.now() }
    })

    const storageBridgeKey = "bridgeSessionID"
    const resolveBridge = async (): Promise<string | undefined> => {
      const stored = (await ctx.storage.get(storageBridgeKey)) as { sessionID?: string } | undefined
      if (stored?.sessionID) {
        try {
          await ctx.session.get({ sessionID: stored.sessionID })
          return stored.sessionID
        } catch {
          // Deleted — fall through and recreate.
        }
      }
      try {
        const created = await ctx.session.create({ title: "Butler" })
        await ctx.storage.set(storageBridgeKey, { sessionID: created.id })
        return created.id
      } catch (err) {
        console.error("[butler] could not create bridge session:", err)
        return undefined
      }
    }

    const resolveTarget = async (): Promise<string | undefined> =>
      opts.bridge ? await resolveBridge() : lastActive?.sessionID ?? (await resolveBridge())

    const titles = new Map<string, string>()
    const sessionTitle = async (inst: ButlerInstance, sid: string): Promise<string> => {
      const cached = titles.get(sid)
      if (cached) return cached
      try {
        const res = await fetch(`${inst.server}/api/v1/sessions/${encodeURIComponent(sid)}`, {
          headers: { authorization: `Bearer ${inst.token}` },
        })
        if (res.ok) {
          const detail = (await res.json()) as { session?: { title?: string } }
          const title = detail?.session?.title ?? sid
          titles.set(sid, title)
          return title
        }
      } catch {
        // Offline or transient — fall back to the raw id.
      }
      return sid
    }

    const relay = async (inst: ButlerInstance, msgs: ButlerMessage[]): Promise<void> => {
      const target = await resolveTarget()
      if (!target) {
        console.error("[butler] no target session — dropping relay")
        return
      }
      const bySession = new Map<string, ButlerMessage[]>()
      for (const m of msgs) {
        const list = bySession.get(m.session_id) ?? []
        list.push(m)
        bySession.set(m.session_id, list)
      }
      for (const [sid, list] of bySession) {
        const title = await sessionTitle(inst, sid)
        const lines = list.map(m =>
          m.kind === "context"
            ? `[${m.seq}] ${m.sender_id} set context: ${m.content}`
            : `[${m.seq}] ${m.sender_id}: ${m.content}`,
        )
        const text = [
          `[butler relay] ${list.length} new message${list.length === 1 ? "" : "s"} in "${title}" (${sid}):`,
          "",
          ...lines,
          "",
          "Relayed automatically by the butler plugin — no human typed this. Reply to the sender with the butler MCP tools (send_message) if a response is needed.",
        ].join("\n")
        try {
          await ctx.session.prompt({ sessionID: target, text, delivery: opts.delivery ?? "queue" })
        } catch (err) {
          console.error(`[butler] relay to session ${target} failed:`, err)
        }
      }
    }

    const controller = new AbortController()
    const aborted = () => controller.signal.aborted

    void (async () => {
      // Phase 1: wait for the state file (butler-mcp may still be starting).
      let instance = readInstance()
      for (let i = 0; !instance && i < 15; i++) {
        await sleep(2_000, controller.signal)
        if (aborted()) return
        instance = readInstance()
      }
      if (!instance) {
        console.error(
          `[butler] no instance state file found (${candidates.join(", ")}); not relaying. ` +
            "Start the butler MCP server once to create it.",
        )
        return
      }
      console.log(`[butler] relaying messages for ${instance.agent} from ${instance.server}`)

      const api = (inst: ButlerInstance, pathAndQuery: string) =>
        fetch(`${inst.server}${pathAndQuery}`, {
          headers: { authorization: `Bearer ${inst.token}` },
          signal: controller.signal,
        })

      // Phase 2: initialize the cursor to the server's latest seq (skip history).
      let cursor = 0
      let backoff = 0
      for (;;) {
        if (aborted()) return
        try {
          const res = await api(instance, "/api/v1/seq")
          if (res.status === 401) throw new Error("401")
          if (!res.ok) throw new Error(`seq: HTTP ${res.status}`)
          cursor = ((await res.json()) as { latest_seq?: number }).latest_seq ?? 0
          break
        } catch (err: any) {
          if (aborted() || err?.name === "AbortError") return
          if (String(err?.message) === "401") {
            const fresh = readInstance()
            if (fresh) instance = fresh
          }
          backoff = Math.min(backoff ? backoff * 2 : 2_000, MAX_BACKOFF_MS)
          console.error(`[butler] seq fetch failed (retry in ${backoff}ms):`, err?.message ?? err)
          await sleep(backoff, controller.signal)
        }
      }

      // Phase 3: long-poll forever.
      backoff = 0
      for (;;) {
        if (aborted()) return
        try {
          const res = await api(instance, `/api/v1/wait?after=${cursor}&timeout=${WAIT_TIMEOUT_SECS}`)
          if (res.status === 401) {
            const fresh = readInstance()
            if (fresh) {
              instance = fresh
              console.log(`[butler] token rotated; now acting as ${instance.agent}`)
            }
            await sleep(1_000, controller.signal)
            continue
          }
          if (!res.ok) throw new Error(`wait: HTTP ${res.status}`)
          const body = (await res.json()) as { messages?: ButlerMessage[], latest_seq?: number }
          backoff = 0
          const latest = body.latest_seq ?? cursor
          const relevant = (body.messages ?? []).filter(
            m => m.sender_id !== instance.agent && m.kind !== "system",
          )
          if (relevant.length > 0) await relay(instance, relevant)
          cursor = Math.max(cursor, latest)
        } catch (err: any) {
          if (aborted() || err?.name === "AbortError") return
          backoff = Math.min(backoff ? backoff * 2 : 2_000, MAX_BACKOFF_MS)
          console.error(`[butler] poll error (retry in ${backoff}ms):`, err?.message ?? err)
          await sleep(backoff, controller.signal)
        }
      }
    })()

    return () => controller.abort()
  },
})
