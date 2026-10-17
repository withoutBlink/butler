// Butler relay plugin — one butler role per OpenCode session.
//
// Vocabulary (matches the panel):
//   - agent client — the app-level butler identity (kind, hostname,
//     workdir) that butler-mcp (the MCP server OpenCode spawns) registers
//     at startup, writing its instance token to
//     <workdir>/.butler/instance.json. Pure backend plumbing: once it has
//     live roles the panel hides it (the roles represent it) and it only
//     serves as the registration conduit and the fallback relay.
//   - role — this plugin's per-OpenCode-session butler agent: the same
//     identity plus the OpenCode session id as the client_session
//     discriminator, sibling-registered via the instance token. Its id is a
//     random UUID (the only identity); its display name is the session
//     title, pushed to butler on renames so the panel stays readable.
//
// Role activation is EAGER:
//   - at startup the plugin asks butler for every role of its agent client
//     (GET /api/v1/agents/roles) and re-registers each one whose OpenCode
//     session still exists — dormant sessions come back online without
//     waiting for a model request. Evicted (kicked) roles stay out until
//     their session itself acts; roles of deleted sessions are skipped.
//   - any session touch activates its role: a model request, a viewed
//     session (session.viewed) or an incoming user prompt
//     (session.inbox.enqueued). Sessions of other locations are ignored —
//     they belong to other agent clients (their own plugin instance).
//
// Delivery:
//   - Each role long-polls /api/v1/wait and relays its messages into ITS
//     OWN OpenCode session only — @mentioning a specific role wakes
//     exactly that session. Read marks advance per role (wait batches mark
//     what reached the session's context).
//   - The plugin also shadows the butler MCP tools (send_message,
//     read_session, …) under their same effective names. Their executors
//     see the calling session's id, so replies and history reads act as
//     that session's role. Sessions without a registered role (and Code
//     Mode calls without session context) fall back to the agent client.
//   - The app-level relay loop (the agent client's own messages → the most
//     recently active session, or a dedicated "Butler" bridge session)
//     keeps serving sessions without a live role; a per-seq delivery map
//     dedupes so the active session never sees a message twice.
//
// Options (when loaded via the `plugins` array in opencode.json):
//   stateFile  — explicit path to the instance.json state file
//   bridge     — always deliver the app-level relay to the "Butler" session
//   delivery   — "queue" (default) or "steer"
//
// Known limitation: deleting an OpenCode session leaves its role behind
// (loop stops, role stays with a dead token; the next startup skips it) —
// kick it from the panel to evict it for good.

import { Plugin } from "@opencode/plugin"
import { existsSync, readFileSync } from "node:fs"
import * as path from "node:path"

interface ButlerInstance {
  agent: string
  token: string
  server: string
}

interface SessionAgent {
  sid: string // OpenCode session id
  agent: string // butler agent id (minted from the title)
  token: string // butler instance token for this agent
  server: string // butler server URL
  title: string
  cursor: number // butler wait cursor for this agent's loop
}

interface ButlerMessage {
  seq: number
  session_id: string
  sender_id: string
  kind: string
  content: string
  mentions?: string[]
  created_at?: number
}

const WAIT_TIMEOUT_SECS = 25
const MAX_BACKOFF_MS = 60_000
const DELIVERED_TRACK = 300 // seqs kept in the delivery-dedup map

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise(resolve => {
    const t = setTimeout(resolve, ms)
    signal?.addEventListener("abort", () => { clearTimeout(t); resolve() }, { once: true })
  })
}

// Which butler MCP tools this plugin shadows (matched against their
// effective registry ids, e.g. "butler_send_message"). wait_for_messages and
// check_messages stay MCP: the relay already delivers per session, and the
// blocking-poll tools carry no session context worth binding.
const SHADOWED = /^(send_message|read_session|list_sessions|set_context|get_context|create_session|join_session)$/
const shadowedId = (id: string) => {
  const m = /^butler_*([^_].*)$/.exec(id)
  return m != null && SHADOWED.test(m[1])
}

export default Plugin.define({
  id: "butler",
  async setup(ctx) {
    const opts = ((ctx as any).options ?? {}) as {
      stateFile?: string
      bridge?: boolean
      delivery?: "queue" | "steer"
    }

    const controller = new AbortController()
    const aborted = () => controller.signal.aborted

    // ---- app-level identity (instance.json, written by butler-mcp) ----

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

    let app: ButlerInstance | undefined
    const getApp = (): ButlerInstance | undefined => app ?? readInstance()

    // One shared wait for the state file (butler-mcp may still be starting).
    const instanceReady = (async () => {
      for (let i = 0; i < 15; i++) {
        const inst = readInstance()
        if (inst) {
          app = inst
          return inst
        }
        if (aborted()) return undefined
        await sleep(2_000, controller.signal)
      }
      return undefined
    })()

    // ---- per-session butler agents ----

    const sessionAgents = new Map<string, SessionAgent>()
    const pending = new Set<string>()
    const titles = new Map<string, string>() // butler session id -> title

    // seq -> OpenCode session ids that already received it; prevents the
    // app-level relay from double-delivering to the active session what a
    // session-agent loop already relayed.
    const delivered = new Map<number, Set<string>>()
    const markDelivered = (seq: number, target: string) => {
      let set = delivered.get(seq)
      if (!set) {
        set = new Set()
        delivered.set(seq, set)
      }
      set.add(target)
      if (delivered.size > DELIVERED_TRACK) {
        let drop = delivered.size - Math.floor(DELIVERED_TRACK / 2)
        for (const key of delivered.keys()) {
          if (drop-- <= 0) break
          delivered.delete(key)
        }
      }
    }

    // Track the most recently active session (any model request in the agent
    // loop) so the app-level relay lands in the conversation being used, and
    // register a session agent on a session's first request.
    let lastActive: { sessionID: string, at: number } | undefined
    await ctx.session.hook("context", (event: any) => {
      const sid = event?.sessionID
      if (typeof sid !== "string") return
      lastActive = { sessionID: sid, at: Date.now() }
      void ensureSessionAgent(sid)
      void syncTitle(sid)
    })

    // A session's title can change (the user renames it). Registered session
    // agents keep their minted id but their display title should follow, so
    // the panel and participant lists stay recognizable: on each model
    // request (throttled) push the current title via the rename endpoint.
    const titleSyncAt = new Map<string, number>()
    const syncTitle = async (sid: string) => {
      const entry = sessionAgents.get(sid)
      if (!entry) return
      if (Date.now() - (titleSyncAt.get(sid) ?? 0) < 30_000) return
      titleSyncAt.set(sid, Date.now())
      try {
        const info = await ctx.session.get({ sessionID: sid })
        const title = info?.title
        if (!title || title === entry.title) return
        const res = await api(entry, "POST", "/api/v1/agents/rename", {
          client_session: sid,
          display_name: title,
        })
        if (res.ok) {
          entry.title = title
          console.log(`[butler] session agent ${entry.agent} retitled to "${title}"`)
        }
      } catch {
        // Transient — the next model request retries.
      }
    }

    const waitForTitle = async (sid: string): Promise<string | undefined> => {
      // Titles are generated shortly after the first exchange; poll until
      // the session has one (or is gone). Sessions of another location are
      // skipped too — they belong to a different agent client (another
      // plugin instance in that directory owns their roles).
      for (let i = 0; i < 30; i++) {
        try {
          const info = await ctx.session.get({ sessionID: sid })
          const dir = (info as any)?.location?.directory
          if (dir && dir !== ctx.location.directory) return undefined
          if (info?.title) return info.title
        } catch {
          return undefined // deleted or unavailable session
        }
        if (aborted()) return undefined
        await sleep(2_000, controller.signal)
      }
      return undefined
    }

    const registerSibling = async (sid: string, title: string): Promise<SessionAgent> => {
      await instanceReady
      let inst = getApp()
      if (!inst) throw new Error("no butler instance state file")
      for (let attempt = 0; ; attempt++) {
        const res = await fetch(`${inst.server}/api/v1/register`, {
          method: "POST",
          headers: { authorization: `Bearer ${inst.token}`, "content-type": "application/json" },
          body: JSON.stringify({ client_session: sid, display_name: title }),
          signal: controller.signal,
        })
        if (res.status === 401 && attempt === 0) {
          // App token rotated (butler-mcp re-registered): re-read and retry.
          const fresh = readInstance()
          if (fresh) {
            app = fresh
            inst = fresh
            continue
          }
        }
        if (!res.ok) throw new Error(`register sibling: HTTP ${res.status}`)
        const body = (await res.json()) as { name: string, token: string }
        return { sid, agent: body.name, token: body.token, server: inst.server, title, cursor: 0 }
      }
    }

    const ensureSessionAgent = async (sid: string) => {
      if (sessionAgents.has(sid) || pending.has(sid)) return
      pending.add(sid)
      try {
        const title = await waitForTitle(sid)
        if (!title) return // gone or still untitled; retried on the next request
        const entry = await registerSibling(sid, title)
        sessionAgents.set(sid, entry)
        console.log(`[butler] session agent ${entry.agent} for "${title}" (${sid})`)
        startSessionLoop(entry)
      } catch (err: any) {
        if (aborted() || err?.name === "AbortError") return
        console.error(`[butler] could not register session agent for ${sid}:`, err?.message ?? err)
      } finally {
        pending.delete(sid)
      }
    }

    const startSessionLoop = (entry: SessionAgent) => {
      void (async () => {
        // The cursor persists in plugin storage: OpenCode tears plugin
        // contexts down when no client work is active, killing every wait
        // loop with them — roles then show offline even though the opencode
        // service process keeps running. A persisted cursor makes the next
        // activation deliver everything that arrived during the dead window
        // instead of skipping it. First run: start at the server's latest
        // seq (skip history; agents use read_session for anything older).
        const cursorKey = `butler:cursor:${entry.sid}`
        let savedCursor = -1
        try {
          const saved = await ctx.storage.get(cursorKey)
          if (typeof saved === "number") savedCursor = saved
        } catch {}
        let backoff = 0
        if (savedCursor >= 0) {
          entry.cursor = savedCursor
        } else {
          for (;;) {
            if (aborted()) return
            try {
              const res = await butlerFetch(entry, "/api/v1/seq")
              if (res.status === 401) throw new Error("401")
              if (!res.ok) throw new Error(`seq: HTTP ${res.status}`)
              const body = (await res.json()) as { latest_seq?: number }
              entry.cursor = body.latest_seq ?? 0
              break
            } catch (err: any) {
              if (aborted() || err?.name === "AbortError") return
              backoff = Math.min(backoff ? backoff * 2 : 2_000, MAX_BACKOFF_MS)
              await sleep(backoff, controller.signal)
            }
          }
        }
        backoff = 0
        for (;;) {
          if (aborted()) return
          try {
            const res = await butlerFetch(entry, `/api/v1/wait?after=${entry.cursor}&timeout=${WAIT_TIMEOUT_SECS}`)
            if (res.status === 401) {
              // Evicted (kicked) or the server DB was reset: stop this
              // loop. The session re-registers on its next model request
              // if it is still alive — re-registering HERE would defeat
              // kicks.
              sessionAgents.delete(entry.sid)
              console.error(`[butler] session agent ${entry.agent} got 401 (evicted?); stopping its loop`)
              return
            }
            if (!res.ok) throw new Error(`wait: HTTP ${res.status}`)
            const body = (await res.json()) as { messages?: ButlerMessage[], latest_seq?: number }
            backoff = 0
            const relevant = (body.messages ?? []).filter(
              m => m.sender_id !== entry.agent && m.kind !== "system",
            )
            // Claim the (seq, session) pairs synchronously before the async
            // relay: the app-level loop may be delivering the same message
            // to this same session right now — whichever claims first
            // delivers, the other skips.
            const todo = relevant.filter(m => !delivered.get(m.seq)?.has(entry.sid))
            for (const m of todo) markDelivered(m.seq, entry.sid)
            if (todo.length > 0) {
              const ok = await relayInto(entry.sid, entry.agent, entry, todo)
              if (!ok) return // target session is gone — stop this loop
            }
            entry.cursor = Math.max(entry.cursor, body.latest_seq ?? entry.cursor)
            if (entry.cursor > savedCursor) {
              // Persist only after the batch was relayed: at-least-once —
              // a teardown between relay and persist may re-deliver a
              // batch, one before the relay retries it.
              savedCursor = entry.cursor
              try {
                await ctx.storage.set(cursorKey, entry.cursor)
              } catch {}
            }
          } catch (err: any) {
            if (aborted() || err?.name === "AbortError") return
            backoff = Math.min(backoff ? backoff * 2 : 2_000, MAX_BACKOFF_MS)
            await sleep(backoff, controller.signal)
          }
        }
      })()
    }

    // ---- HTTP helpers ----

    // Any object with a server+token (app instance or session agent).
    const butlerFetch = (
      ident: { server: string, token: string },
      pathAndQuery: string,
      init?: RequestInit,
    ) =>
      fetch(`${ident.server}${pathAndQuery}`, {
        ...init,
        headers: {
          authorization: `Bearer ${ident.token}`,
          ...(init?.body ? { "content-type": "application/json" } : {}),
          ...(init?.headers as Record<string, string> | undefined),
        },
        signal: controller.signal,
      })

    const sessionTitle = async (ident: { server: string, token: string }, sid: string): Promise<string> => {
      const cached = titles.get(sid)
      if (cached) return cached
      try {
        const res = await butlerFetch(ident, `/api/v1/sessions/${encodeURIComponent(sid)}`)
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

    // ---- relay into an OpenCode session ----

    const relayInto = async (
      target: string,
      myAgent: string,
      ident: { server: string, token: string },
      msgs: ButlerMessage[],
    ): Promise<boolean> => {
      const isMentioned = (m: ButlerMessage) => (m.mentions ?? []).includes(myAgent)
      const bySession = new Map<string, ButlerMessage[]>()
      for (const m of msgs) {
        const list = bySession.get(m.session_id) ?? []
        list.push(m)
        bySession.set(m.session_id, list)
      }
      for (const [sid, list] of bySession) {
        const title = await sessionTitle(ident, sid)
        const lines = list.map(m =>
          m.kind === "context"
            ? `${isMentioned(m) ? "ADDRESSED TO YOU (reply expected): " : "FYI: "}[${m.seq}] ${m.sender_id} set context: ${m.content}`
            : `${isMentioned(m) ? "ADDRESSED TO YOU (reply expected): " : "FYI: "}[${m.seq}] ${m.sender_id}: ${m.content}`,
        )
        const mentioned = list.filter(isMentioned).length
        const urgency =
          mentioned > 0
            ? `${mentioned} of these message(s) @-mention you — respond to those with the butler send_message tool. The rest are FYI. If you decide to act on any of them, ack briefly first (send_message "on it"), then do the work and report back — don't stay silent until finished.`
            : "None of these messages @-mention you — FYI only; reply with the butler send_message tool if a response is needed. If you do decide to act on one, ack briefly first, then do the work and report back — don't stay silent until finished."
        const isSessionAgent = sessionAgents.get(target)?.agent === myAgent
        const text = [
          `[butler relay] ${list.length} new message${list.length === 1 ? "" : "s"} in "${title}" (${sid}):`,
          "",
          ...lines,
          "",
          `You are ${myAgent} in butler${isSessionAgent ? " (one agent per OpenCode session)" : " — the shared app-level identity; this OpenCode session also has its own agent"}.`,
          "Relayed automatically by the butler plugin — no human typed this.",
          urgency,
        ].join("\n")
        try {
          await ctx.session.prompt({ sessionID: target, text, delivery: opts.delivery ?? "queue" })
        } catch (err: any) {
          if (/not found|404|no such|deleted/i.test(String(err?.message ?? err))) {
            sessionAgents.delete(target)
            console.error(`[butler] target session ${target} is gone; stopping its agent loop`)
            return false
          }
          console.error(`[butler] relay to session ${target} failed:`, err)
        }
      }
      return true
    }

    // ---- app-level relay (shared identity → most recently active session) ----

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

    // ---- eager role activation ----
    //
    // The butler server remembers every role (client_session) this agent
    // client ever registered — ids are stable — so at startup we ask for
    // the list and re-activate each role whose OpenCode session still
    // exists. Sessions that are open but dormant come back online without
    // waiting for a model request. Evicted (kicked) roles stay out until
    // their session itself acts; roles whose session no longer exists fail
    // ctx.session.get inside ensureSessionAgent and are skipped.
    void (async () => {
      const inst = await instanceReady
      if (!inst) return
      for (;;) {
        if (aborted()) return
        try {
          const res = await butlerFetch(getApp()!, "/api/v1/agents/roles")
          if (res.status === 401) {
            const fresh = readInstance()
            if (fresh) {
              app = fresh
              continue
            }
          }
          if (!res.ok) throw new Error(`roles: HTTP ${res.status}`)
          const body = (await res.json()) as {
            roles?: { client_session: string, agent: string, removed?: boolean }[]
          }
          let activated = 0
          for (const r of body.roles ?? []) {
            if (!r.client_session || r.removed) continue
            void ensureSessionAgent(r.client_session)
            activated++
          }
          console.log(`[butler] activated ${activated} role(s) of ${inst.agent}`)
          return
        } catch (err: any) {
          if (aborted() || err?.name === "AbortError") return
          // butler down or the client token dead (kicked client, waiting
          // for butler-mcp's next registration) — retry quietly.
          await sleep(30_000, controller.signal)
        }
      }
    })()

    // Roles also activate on ANY session touch, not just model requests: a
    // session the user opens (session.viewed) or messages
    // (session.inbox.enqueued) gets its role registered even if the model
    // never runs. ensureSessionAgent skips sessions belonging to other
    // locations — those are other agent clients' sessions.
    void (async () => {
      try {
        const stream = (ctx.event as any).subscribe({ signal: controller.signal }) as AsyncIterable<any>
        for await (const ev of stream) {
          const type = ev?.type
          if (type !== "session.viewed" && type !== "session.inbox.enqueued") continue
          const sid = ev?.data?.sessionID
          if (typeof sid !== "string") continue
          const dir = ev?.location?.directory
          if (dir && dir !== ctx.location.directory) continue
          void ensureSessionAgent(sid)
        }
      } catch {
        // stream aborted on unload
      }
    })()

    void (async () => {
      const instance = await instanceReady
      if (!instance) {
        console.error(
          `[butler] no instance state file found (${candidates.join(", ")}); not relaying. ` +
            "Start the butler MCP server once to create it.",
        )
        return
      }
      console.log(`[butler] relaying messages for ${instance.agent} from ${instance.server}`)

      // Initialize the shared cursor to the server's latest seq (skip history).
      let cursor = 0
      let backoff = 0
      for (;;) {
        if (aborted()) return
        try {
          const res = await butlerFetch(getApp()!, "/api/v1/seq")
          if (res.status === 401) throw new Error("401")
          if (!res.ok) throw new Error(`seq: HTTP ${res.status}`)
          const body = (await res.json()) as { latest_seq?: number }
          cursor = body.latest_seq ?? 0
          break
        } catch (err: any) {
          if (aborted() || err?.name === "AbortError") return
          if (String(err?.message) === "401") app = readInstance() ?? app
          backoff = Math.min(backoff ? backoff * 2 : 2_000, MAX_BACKOFF_MS)
          await sleep(backoff, controller.signal)
        }
      }

      // Long-poll forever. Messages a session-agent loop already delivered
      // to the resolved target are skipped (per-seq dedup).
      backoff = 0
      for (;;) {
        if (aborted()) return
        try {
          const current = getApp()!
          const res = await butlerFetch(current, `/api/v1/wait?after=${cursor}&timeout=${WAIT_TIMEOUT_SECS}`)
          if (res.status === 401) {
            const fresh = readInstance()
            if (fresh) {
              app = fresh
              console.log(`[butler] token rotated; now acting as ${app.agent}`)
            }
            await sleep(1_000, controller.signal)
            continue
          }
          if (!res.ok) throw new Error(`wait: HTTP ${res.status}`)
          const body = (await res.json()) as { messages?: ButlerMessage[], latest_seq?: number }
          backoff = 0
          const latest = body.latest_seq ?? cursor
          const relevant = (body.messages ?? []).filter(
            m => m.sender_id !== current.agent && m.kind !== "system",
          )
          if (relevant.length > 0) {
            const target = await resolveTarget()
            if (!target) {
              console.error("[butler] no target session — dropping relay")
            } else {
              // Same claim-first dedup as the session loops: mark the
              // (seq, target) pairs synchronously, then relay — a
              // session-agent loop may be racing us to the same session.
              // Also never echo a message back into the session whose own
              // agent authored it: that session's loop skips self-messages,
              // and the author seeing their own words relayed back is noise,
              // not information.
              const self = sessionAgents.get(target)?.agent
              const fresh = relevant.filter(
                m => !delivered.get(m.seq)?.has(target) && m.sender_id !== self,
              )
              for (const m of fresh) markDelivered(m.seq, target)
              if (fresh.length > 0) await relayInto(target, current.agent, current, fresh)
            }
          }
          cursor = Math.max(cursor, latest)
        } catch (err: any) {
          if (aborted() || err?.name === "AbortError") return
          backoff = Math.min(backoff ? backoff * 2 : 2_000, MAX_BACKOFF_MS)
          console.error(`[butler] poll error (retry in ${backoff}ms):`, err?.message ?? err)
          await sleep(backoff, controller.signal)
        }
      }
    })()

    // ---- shadowed butler tools ----
    //
    // Same effective names as the butler MCP tools, so agents (and their
    // instructions) keep working unchanged — but the executors bind to the
    // calling session's butler identity. The MCP originals are removed in
    // the transform; if this plugin is missing, they simply stay.

    const identityFor = (sessionID: unknown): { server: string, token: string, agent: string } => {
      if (typeof sessionID === "string") {
        const e = sessionAgents.get(sessionID)
        if (e) return { server: e.server, token: e.token, agent: e.agent }
      }
      const inst = getApp()
      return { server: inst?.server ?? "", token: inst?.token ?? "", agent: inst?.agent ?? "" }
    }

    const api = async (
      ident: { server: string, token: string },
      method: string,
      pathAndQuery: string,
      body?: unknown,
    ): Promise<Response> => {
      const init: RequestInit = body !== undefined ? { method, body: JSON.stringify(body) } : { method }
      let res: Response
      try {
        res = await butlerFetch(ident, pathAndQuery, init)
      } catch (err: any) {
        throw new Error(err?.message ?? String(err))
      }
      if (res.status === 401) {
        // Tokens rotate on re-registration. For the APP identity, re-read
        // the state file and retry once (it cannot re-register itself —
        // only butler-mcp holds the kind token). For SESSION agents, an
        // evicted/kicked token must not self-recover: drop the entry — the
        // session re-registers on its next model request.
        const sid = [...sessionAgents.entries()].find(([, e]) => e.token === ident.token)?.[0]
        if (sid) {
          sessionAgents.delete(sid)
          return res
        }
        const freshInst = readInstance()
        if (freshInst) {
          app = freshInst
          return await butlerFetch({ server: freshInst.server, token: freshInst.token }, pathAndQuery, init)
        }
      }
      return res
    }

    const errText = (msg: string) => ({ content: `butler error: ${msg}` })
    const errBody = async (res: Response): Promise<string> => {
      try {
        const body = (await res.json()) as { error?: string }
        return body?.error ?? `HTTP ${res.status}`
      } catch {
        return `HTTP ${res.status}`
      }
    }
    const fmtTime = (ms?: number) =>
      (ms ? new Date(ms) : new Date()).toISOString().slice(11, 19)

    await ctx.tool.transform((editor: any) => {
      editor.namespace({
        name: "butler",
        description:
          "Butler messaging tools. Calls act as the butler identity of THIS OpenCode session — every session has its own agent — falling back to the shared app identity when this session has none yet.",
      })
      for (const t of editor.list()) {
        if (shadowedId(t.id)) editor.remove(t.id)
      }

      const exec = (fn: (input: any, context: any) => Promise<{ content: string }>) => fn

      editor.add({
        name: "send_message",
        description:
          'Send a chat message to a session as your session\'s butler identity. You must be a participant (join_session first). Address specific agents with @mentions: "@<agent id>" for one agent, "@<kind>" for every agent of that kind in the session, or "@all" for all participants. Mentioned agents are expected to reply; unmentioned participants only get the message as FYI. Returns the message sequence number.',
        input: {
          type: "object",
          properties: {
            session: { type: "string", description: "session id, required" },
            content: { type: "string", description: "message text (markdown), required" },
          },
          required: ["session", "content"],
          additionalProperties: false,
        },
        options: { namespace: "butler", codemode: true },
        execute: exec(async (input, context) => {
          const { session, content } = input ?? {}
          if (!session) return errText("session is required")
          if (!String(content ?? "").trim()) return errText("content is required")
          const ident = identityFor(context?.sessionID)
          const res = await api(ident, "POST", `/api/v1/sessions/${encodeURIComponent(session)}/messages`, { content })
          if (!res.ok) return errText(await errBody(res))
          const msg = (await res.json()) as { seq: number }
          return { content: `sent message seq=${msg.seq}` }
        }),
      })

      editor.add({
        name: "read_session",
        description:
          "Read a session: title, participants (with presence) and the most recent messages (default 50, newest last), as your session's butler identity. Use to catch up on a conversation or check shared context.",
        input: {
          type: "object",
          properties: {
            session: { type: "string", description: "session id, required" },
            last_n: { type: "number", description: "number of recent messages to return, default 50" },
          },
          required: ["session"],
          additionalProperties: false,
        },
        options: { namespace: "butler", codemode: true },
        execute: exec(async (input, context) => {
          const { session } = input ?? {}
          if (!session) return errText("session is required")
          let n = Number((input as any)?.last_n ?? 0)
          if (!Number.isFinite(n) || n <= 0) n = 50
          if (n > 500) n = 500
          const ident = identityFor(context?.sessionID)
          const detailRes = await api(ident, "GET", `/api/v1/sessions/${encodeURIComponent(session)}`)
          if (!detailRes.ok) return errText(await errBody(detailRes))
          const detail = (await detailRes.json()) as {
            session?: { id: string, title: string, status: string, created_by: string },
            participants?: { id: string, kind: string, hostname: string, online?: boolean }[],
          }
          const msgRes = await api(ident, "GET", `/api/v1/sessions/${encodeURIComponent(session)}/messages?latest=${n}`)
          if (!msgRes.ok) return errText(await errBody(msgRes))
          const { messages = [] } = (await msgRes.json()) as { messages: ButlerMessage[] }

          const sess = detail.session
          const names = (detail.participants ?? []).map(p => {
            let tag = p.id
            if (p.hostname) tag += ` (${p.kind}@${p.hostname})`
            else if (p.kind) tag += ` (${p.kind})`
            if (p.online) tag += "*"
            return tag
          })
          const lines = [
            `session "${sess?.title}" (${sess?.id ?? session}) [${sess?.status ?? "?"}]`,
            `created by ${sess?.created_by ?? "?"}`,
            `participants (online*): ${names.join(", ")}`,
          ]
          if (messages.length === 0) {
            lines.push("no messages yet")
            return { content: lines.join("\n") }
          }
          lines.push(`last ${messages.length} message(s):`)
          for (const m of messages) {
            let sender = m.sender_id
            if (m.kind !== "chat") sender += ` (${m.kind})`
            if ((m.mentions ?? []).includes(ident.agent)) sender += " (mentioned you)"
            lines.push(`[${m.seq} ${fmtTime(m.created_at)}] ${sender}: ${m.content}`)
          }
          return { content: lines.join("\n") }
        }),
      })

      editor.add({
        name: "list_sessions",
        description:
          "List Butler messaging sessions as your session's butler identity. The \"global\" session has every connected agent as a member — use it to reach everyone. Also shows your other joined sessions and active sessions you can join. Use this first to discover where to send messages.",
        input: { type: "object", properties: {}, additionalProperties: false },
        options: { namespace: "butler", codemode: true },
        execute: exec(async (_input, context) => {
          const ident = identityFor(context?.sessionID)
          const joinedRes = await api(ident, "GET", "/api/v1/sessions?joined=1")
          if (!joinedRes.ok) return errText(await errBody(joinedRes))
          const allRes = await api(ident, "GET", "/api/v1/sessions")
          if (!allRes.ok) return errText(await errBody(allRes))
          const { sessions: joined = [] } = (await joinedRes.json()) as { sessions: { id: string, title: string, status: string, created_at?: number }[] }
          const { sessions: all = [] } = (await allRes.json()) as { sessions: { id: string, title: string, status: string, created_at?: number }[] }
          const joinedSet = new Set(joined.map(s => s.id))
          const other = all.filter(s => s.status === "active" && !joinedSet.has(s.id))
          joined.sort((a, b) => (b.created_at ?? 0) - (a.created_at ?? 0))
          const lines = [`you are ${ident.agent}`]
          if (joined.length === 0) {
            lines.push("you have not joined any sessions; create one with create_session or join one below")
          } else {
            lines.push(`your sessions (${joined.length}):`)
            for (const s of joined) lines.push(`  ${s.id}  "${s.title}"  [${s.status}]`)
          }
          if (other.length > 0) {
            lines.push(`other active sessions you can join (${other.length}):`)
            for (const s of other) lines.push(`  ${s.id}  "${s.title}"`)
          }
          return { content: lines.join("\n") }
        }),
      })

      editor.add({
        name: "set_context",
        description:
          "Set or update a shared context item (key/value note) on a session, visible to all participants, as your session's butler identity. Use for durable facts other agents should know: decisions, links, task state. Updates are announced in the session message stream.",
        input: {
          type: "object",
          properties: {
            session: { type: "string", description: "session id, required" },
            key: { type: "string", description: "short key, e.g. decision_auth_lib or current_status" },
            value: { type: "string", description: "value text for the key" },
          },
          required: ["session", "key", "value"],
          additionalProperties: false,
        },
        options: { namespace: "butler", codemode: true },
        execute: exec(async (input, context) => {
          const { session, key, value } = input ?? {}
          if (!session) return errText("session is required")
          if (!String(key ?? "").trim() || !String(value ?? "").trim()) return errText("key and value are required")
          const ident = identityFor(context?.sessionID)
          const res = await api(ident, "PUT", `/api/v1/sessions/${encodeURIComponent(session)}/context/${encodeURIComponent(key)}`, { value })
          if (!res.ok) return errText(await errBody(res))
          return { content: `context ${key} updated` }
        }),
      })

      editor.add({
        name: "get_context",
        description:
          "Read the shared context items (key/value notes) of a session as your session's butler identity: durable session-level facts like decisions, file paths, task state. Different from chat history — this is structured info all participants share.",
        input: {
          type: "object",
          properties: {
            session: { type: "string", description: "session id, required" },
          },
          required: ["session"],
          additionalProperties: false,
        },
        options: { namespace: "butler", codemode: true },
        execute: exec(async (input, context) => {
          const { session } = input ?? {}
          if (!session) return errText("session is required")
          const ident = identityFor(context?.sessionID)
          const res = await api(ident, "GET", `/api/v1/sessions/${encodeURIComponent(session)}/context`)
          if (!res.ok) return errText(await errBody(res))
          const { items = [] } = (await res.json()) as { items: { key: string, value: string }[] }
          if (items.length === 0) return { content: "no context items set for this session" }
          const lines = [`${items.length} context item(s):`]
          for (const item of items) lines.push(`${item.key} = ${item.value}`)
          return { content: lines.join("\n") }
        }),
      })

      editor.add({
        name: "create_session",
        description:
          'Create a new messaging session for a task and optionally invite other agents by name (find them in list_sessions/read_session participant lists; "admin" is the human on the panel), as your session\'s butler identity. You are added as a participant automatically. Returns the session id to use with the other tools.',
        input: {
          type: "object",
          properties: {
            title: { type: "string", description: "short title for the task or conversation, required" },
            participants: { type: "array", items: { type: "string" }, description: "agent ids to invite" },
          },
          required: ["title"],
          additionalProperties: false,
        },
        options: { namespace: "butler", codemode: true },
        execute: exec(async (input, context) => {
          const { title, participants } = input ?? {}
          if (!String(title ?? "").trim()) return errText("title is required")
          const ident = identityFor(context?.sessionID)
          const res = await api(ident, "POST", "/api/v1/sessions", {
            title,
            ...(Array.isArray(participants) && participants.length > 0 ? { participants } : {}),
          })
          if (!res.ok) return errText(await errBody(res))
          const sess = (await res.json()) as { id: string, title: string }
          return {
            content: `created session "${sess.title}" (${sess.id})\nyou are a participant; others can join it via list_sessions + join_session`,
          }
        }),
      })

      editor.add({
        name: "join_session",
        description:
          "Join a session so you can read and send messages in it, as your session's butler identity. Idempotent. After joining, call read_session to catch up on history.",
        input: {
          type: "object",
          properties: {
            session: { type: "string", description: "session id from list_sessions, required" },
          },
          required: ["session"],
          additionalProperties: false,
        },
        options: { namespace: "butler", codemode: true },
        execute: exec(async (input, context) => {
          const { session } = input ?? {}
          if (!session) return errText("session is required")
          const ident = identityFor(context?.sessionID)
          const res = await api(ident, "POST", `/api/v1/sessions/${encodeURIComponent(session)}/join`)
          if (!res.ok) return errText(await errBody(res))
          const { joined } = (await res.json()) as { joined: boolean }
          if (joined) return { content: `joined session ${session}\ncall read_session to catch up on its history` }
          return { content: `already a participant of session ${session}` }
        }),
      })
    })

    return () => controller.abort()
  },
})
