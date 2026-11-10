// Run: node --experimental-strip-types --test plugins/pi/relay.test.ts
// (Node 22 strips the types. Pi itself loads the plugin with jiti.)

import assert from "node:assert/strict"
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import * as path from "node:path"
import { test } from "node:test"

import { startRelay, type PiSender } from "./index.ts"
import {
  HANDOFF_GRACE_MS,
  STUCK_MS,
  SWEEP_KEY,
  initialCursor,
  loadConfig,
  mentionPointer,
  note,
  planDelivery,
  settle,
  sweepPointer,
  userPromptText,
  type Slot,
} from "./relay.ts"

const TASK_ID = "11111111-1111-1111-1111-111111111111"

test("initialCursor replays a reset stream and keeps a cursor that is still behind", () => {
  assert.equal(initialCursor(undefined, 40), 40)
  assert.equal(initialCursor(10, 40), 10)
  assert.equal(initialCursor(0, 0), 0)
  assert.equal(initialCursor(80, 3), 0)
})

test("mention pointer names the session and does not paste the message body", () => {
  const text = mentionPointer("agent-1", "global", "Global", [
    "please review the secret phrase PURPLE-OTTER",
    `assigned task "Ship it" (${TASK_ID}) to you`,
  ])
  assert.match(text, /\[butler\] new messages in "Global" \(global\)\./)
  assert.equal(text.includes("PURPLE-OTTER"), false)
  assert.match(text, new RegExp(TASK_ID))
  assert.match(text, /Ship it/)
  assert.match(text, /read_session/)
  assert.match(text, /mcp__butler__send_message/)
  assert.match(text, /Never set a task done/)
  assert.match(text, /Ask @Noodle in Butler, not Andrew/)
  assert.match(text, /needs Andrew/)
  assert.match(text, /in_progress/)
  assert.match(text, /handoff_task/)
  assert.equal(text.trim().endsWith("butler-notify:global"), true)
})

test("sweep pointer lists open tasks and is its own dedupe key", () => {
  const text = sweepPointer("agent-1", [{ id: "t1", status: "todo", title: "Write the plugin" }])
  assert.match(text, /you own 1 open task/)
  assert.match(text, /t1 \[todo\] "Write the plugin"/)
  assert.equal(text.trim().endsWith(`butler-notify:${SWEEP_KEY}`), true)
})

test("user prompt text joins text blocks and ignores other roles", () => {
  assert.equal(userPromptText({ role: "assistant", content: "nope" }), "")
  assert.equal(userPromptText({ role: "user", content: "plain" }), "plain")
  assert.equal(userPromptText({
    role: "user",
    content: [{ type: "text", text: "line" }, { type: "image", data: "x" }, { type: "text", text: "two" }],
  }), "line\ntwo")
})

test("dedupe holds one slot per key until the prompt that starts handling it", () => {
  const slots = new Map<string, Slot>()
  const first = "first\nbutler-notify:global"
  const second = "second\nbutler-notify:global"
  assert.equal(note(slots, "global", first), "updated")
  assert.equal(note(slots, "global", second), "updated")
  assert.equal(slots.get("global")?.text, second)

  const idle = planDelivery(slots, 1_000, {
    streaming: false, arming: false, idle: true, pendingMessages: false, reclaimHanded: false,
  })
  assert.equal(idle.arming, true)
  assert.deepEqual(idle.sends.map(s => s.deliverAs), [undefined])
  assert.equal(note(slots, "global", "third\nbutler-notify:global"), "skipped")

  assert.deepEqual(settle(slots, idle.sends[0].text), ["global"])
  assert.equal(slots.size, 0)
})

test("a busy run gets followUp for every pending session, and one idle send does not start two turns", () => {
  const slots = new Map<string, Slot>()
  note(slots, "a", "pointer-a")
  note(slots, "b", "pointer-b")
  const idle = planDelivery(slots, 5, {
    streaming: false, arming: false, idle: true, pendingMessages: false, reclaimHanded: false,
  })
  assert.equal(idle.sends.length, 1)
  assert.equal(idle.sends[0].key, "a")
  assert.equal(idle.sends[0].deliverAs, undefined)
  assert.equal(slots.get("b")?.handed, false)

  const rest = planDelivery(slots, 6, {
    streaming: true, arming: true, idle: false, pendingMessages: false, reclaimHanded: false,
  })
  assert.equal(rest.arming, false)
  assert.deepEqual(rest.sends.map(s => [s.key, s.deliverAs]), [["b", "followUp"]])
})

test("a handed pointer is retried once Pi is idle and the queue was cleared", () => {
  const slots = new Map<string, Slot>()
  note(slots, "global", "pointer")
  planDelivery(slots, 0, {
    streaming: false, arming: false, idle: true, pendingMessages: false, reclaimHanded: false,
  })
  const tooSoon = planDelivery(slots, HANDOFF_GRACE_MS - 1, {
    streaming: false, arming: true, idle: true, pendingMessages: false, reclaimHanded: false,
  })
  assert.equal(tooSoon.sends.length, 0)

  const retry = planDelivery(slots, HANDOFF_GRACE_MS, {
    streaming: false, arming: true, idle: true, pendingMessages: false, reclaimHanded: false,
  })
  assert.equal(retry.sends.length, 1)
  assert.equal(retry.sends[0].deliverAs, undefined)

  const queued = new Map<string, Slot>()
  note(queued, "other", "queued")
  const handedAt = 10
  planDelivery(queued, handedAt, {
    streaming: true, arming: false, idle: false, pendingMessages: false, reclaimHanded: false,
  })
  const held = planDelivery(queued, handedAt + STUCK_MS, {
    streaming: true, arming: false, idle: false, pendingMessages: true, reclaimHanded: false,
  })
  assert.equal(held.sends.length, 0, "a follow-up still in Pi's queue is not duplicated")

  const lost = planDelivery(queued, handedAt + STUCK_MS, {
    streaming: true, arming: false, idle: false, pendingMessages: false, reclaimHanded: false,
  })
  assert.equal(lost.sends.length, 1)
  assert.equal(lost.sends[0].deliverAs, "followUp")
})

test("loadConfig reads the pi json file and lets env override it", () => {
  const home = mkdtempSync(path.join(tmpdir(), "butler-pi-"))
  try {
    const dir = path.join(home, ".config", "butler", "pi")
    mkdirSync(dir, { recursive: true })
    writeFileSync(path.join(dir, "plugin.json"), JSON.stringify({
      stateFile: "~/custom/instance.json",
      waitTimeoutSecs: 99,
      sweep: false,
    }))
    const loaded = loadConfig(home, {})
    assert.equal(loaded.warning, undefined)
    assert.equal(loaded.config.stateFile, path.join(home, "custom", "instance.json"))
    assert.equal(loaded.config.waitTimeoutSecs, 60)
    assert.equal(loaded.config.sweep, false)
    const overridden = loadConfig(home, {
      BUTLER_STATE_FILE: "/tmp/other.json",
      BUTLER_PI_SWEEP: "1",
      BUTLER_WAIT_TIMEOUT: "10",
    })
    assert.equal(overridden.config.stateFile, "/tmp/other.json")
    assert.equal(overridden.config.sweep, true)
    assert.equal(overridden.config.waitTimeoutSecs, 10)
  } finally {
    rmSync(home, { recursive: true, force: true })
  }
})

interface Sent { text: string, deliverAs?: string }

function waitFor(pred: () => boolean, label: string): Promise<void> {
  const start = Date.now()
  return new Promise((resolve, reject) => {
    const tick = () => {
      if (pred()) return resolve()
      if (Date.now() - start > 2000) return reject(new Error(`timeout: ${label}`))
      setTimeout(tick, 10)
    }
    tick()
  })
}

test("the relay injects one pointer per butler session and follows up when busy", async () => {
  const home = mkdtempSync(path.join(tmpdir(), "butler-pi-relay-"))
  const stateFile = path.join(home, "instance.json")
  const relayStateFile = path.join(home, "relay-state.json")
  writeFileSync(stateFile, JSON.stringify({ agent: "pi-agent", token: "old-token", server: "http://butler.test" }))

  const sent: Sent[] = []
  const pi: PiSender = {
    sendUserMessage(text, options) {
      sent.push({ text, deliverAs: options?.deliverAs })
    },
  }
  let idle = true
  let pending = false
  const ctx = {
    isIdle: () => idle,
    hasPendingMessages: () => pending,
    ui: { setStatus() {} },
  }
  let release: ((body: unknown) => void) | undefined
  let sawToken = ""
  const titles: Record<string, string> = { global: "Global", room: "Room" }
  const waits: unknown[] = [{
    messages: [
      { seq: 4, session_id: "global", sender_id: "other", kind: "chat", content: "secret phrase PURPLE-OTTER", mentions: ["pi-agent"] },
      { seq: 5, session_id: "global", sender_id: "pi-agent", kind: "chat", content: "my own", mentions: ["pi-agent"] },
      { seq: 6, session_id: "global", sender_id: "other", kind: "system", content: "joined", mentions: ["pi-agent"] },
      { seq: 7, session_id: "room", sender_id: "other", kind: "chat", content: "fyi only", mentions: [] },
      { seq: 8, session_id: "room", sender_id: "other", kind: "chat", content: `task "Ship it" (${TASK_ID})`, mentions: ["pi-agent"] },
    ],
    latest_seq: 8,
  }]

  const fetchImpl: typeof fetch = async (input, init) => {
    const url = String(input)
    const auth = new Headers(init?.headers).get("authorization") ?? ""
    if (url.endsWith("/api/v1/seq")) return Response.json({ latest_seq: 3 })
    if (url.includes("/api/v1/sessions/")) {
      const sid = decodeURIComponent(url.split("/sessions/")[1] ?? "")
      return Response.json({ session: { title: titles[sid] ?? sid } })
    }
    if (url.includes("/api/v1/wait")) {
      if (auth.includes("old-token")) {
        writeFileSync(stateFile, JSON.stringify({ agent: "pi-agent", token: "new-token", server: "http://butler.test" }))
        return new Response("{}", { status: 401 })
      }
      sawToken = auth
      const next = waits.shift()
      if (next) return Response.json(next)
      return new Promise((resolve, reject) => {
        const onAbort = () => reject(Object.assign(new Error("aborted"), { name: "AbortError" }))
        init?.signal?.addEventListener("abort", onAbort, { once: true })
        release = body => {
          init?.signal?.removeEventListener("abort", onAbort)
          resolve(Response.json(body))
        }
      })
    }
    throw new Error(`unexpected ${url}`)
  }

  const relay = startRelay({
    pi,
    ctx,
    fetch: fetchImpl,
    config: { stateFile, relayStateFile, waitTimeoutSecs: 25, sweep: false },
    log() {},
    warn(msg) { throw new Error(msg) },
  })
  try {
    await waitFor(() => {
      try { return JSON.parse(readFileSync(relayStateFile, "utf8")).cursor === 8 } catch { return false }
    }, "first batch cursor")
    assert.match(sawToken, /new-token/)
    assert.equal(sent.length, 1, "one idle turn, the other session waits for the run")
    assert.equal(sent[0].deliverAs, undefined)
    assert.match(sent[0].text, /\[butler\] new messages in "Global" \(global\)\./)
    assert.equal(sent[0].text.includes("PURPLE-OTTER"), false)
    assert.equal(sent[0].text.includes("fyi only"), false)
    const saved = JSON.parse(readFileSync(relayStateFile, "utf8")) as { cursor?: number }
    assert.equal(saved.cursor, 8)

    idle = false
    relay.onRunStart(ctx, { messageType: "user", prompt: sent[0].text })
    await waitFor(() => sent.length >= 2, "followUp for the second session")
    assert.equal(sent[1].deliverAs, "followUp")
    assert.match(sent[1].text, /\[butler\] new messages in "Room" \(room\)\./)
    assert.match(sent[1].text, new RegExp(TASK_ID))
    assert.equal(sent[1].text.includes("Ship it"), true)

    // Same session again while that pointer is handed: dropped.
    const before = sent.length
    waits.push({
      messages: [
        { seq: 9, session_id: "room", sender_id: "other", kind: "chat", content: "still secret ORANGE-FOX", mentions: ["pi-agent"] },
      ],
      latest_seq: 9,
    })
    if (!release) throw new Error("wait did not park")
    const parked = release
    release = undefined
    parked(waits.shift())
    await waitFor(() => {
      try { return JSON.parse(readFileSync(relayStateFile, "utf8")).cursor === 9 } catch { return false }
    }, "cursor advanced past the deduped batch")
    assert.equal(sent.length, before)
    assert.equal(sent[1].text.includes("ORANGE-FOX"), false)

    relay.onMessageStart({ role: "user", content: sent[1].text })
    waits.push({
      messages: [
        { seq: 10, session_id: "room", sender_id: "other", kind: "chat", content: "after the turn", mentions: ["pi-agent"] },
      ],
      latest_seq: 10,
    })
    await waitFor(() => release !== undefined, "next wait parked")
    release!(waits.shift())
    release = undefined
    await waitFor(() => sent.length >= 3, "pointer after the agent started the previous one")
    assert.equal(sent[2].deliverAs, "followUp")
    assert.match(sent[2].text, /Room/)
  } finally {
    relay.stop()
    rmSync(home, { recursive: true, force: true })
  }
})

test("startup sweep injects open tasks and a reset cursor replays from zero", async () => {
  const home = mkdtempSync(path.join(tmpdir(), "butler-pi-sweep-"))
  const stateFile = path.join(home, "instance.json")
  const relayStateFile = path.join(home, "relay-state.json")
  writeFileSync(stateFile, JSON.stringify({ agent: "pi-agent", token: "tok", server: "http://butler.test" }))
  writeFileSync(relayStateFile, JSON.stringify({ cursor: 50 }))
  const sent: Sent[] = []
  const urls: string[] = []
  const fetchImpl: typeof fetch = async (input, init) => {
    const url = String(input)
    urls.push(url)
    if (url.endsWith("/api/v1/seq")) return Response.json({ latest_seq: 2 })
    if (url.includes("/api/v1/tasks")) {
      return Response.json({ tasks: [
        { id: "t-open", status: "in_progress", title: "Keep going" },
        { id: "t-done", status: "done", title: "Finished" },
      ] })
    }
    if (url.includes("/api/v1/wait")) {
      return new Promise((_resolve, reject) => {
        init?.signal?.addEventListener("abort", () => {
          reject(Object.assign(new Error("aborted"), { name: "AbortError" }))
        }, { once: true })
      })
    }
    throw new Error(`unexpected ${url}`)
  }
  const relay = startRelay({
    pi: { sendUserMessage: (text, options) => sent.push({ text, deliverAs: options?.deliverAs }) },
    ctx: { isIdle: () => true, hasPendingMessages: () => false },
    fetch: fetchImpl,
    config: { stateFile, relayStateFile, waitTimeoutSecs: 25, sweep: true },
    log() {},
    warn(msg) {
      if (!msg.includes("server stream restarted")) throw new Error(msg)
    },
  })
  try {
    await waitFor(() => sent.length === 1, "sweep pointer")
    assert.match(sent[0].text, /you own 1 open task/)
    assert.match(sent[0].text, /t-open \[in_progress\]/)
    assert.equal(sent[0].text.includes("Finished"), false)
    assert.equal(sent[0].deliverAs, undefined)
    assert.ok(urls.some(u => u.includes("/api/v1/wait?after=0")))
    assert.equal(JSON.parse(readFileSync(relayStateFile, "utf8")).cursor, 0)
  } finally {
    relay.stop()
    rmSync(home, { recursive: true, force: true })
  }
})
