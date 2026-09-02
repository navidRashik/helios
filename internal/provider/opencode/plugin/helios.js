// helios opencode plugin.
//
// Rendered and installed by internal/provider/opencode/install.go, which
// substitutes the __HELIOS_*__ placeholders below. Edit this file, not the
// installed copy: the installer overwrites what it wrote last time.
//
// Why a plugin and not a hook table. opencode has no hooks.json: the only way
// in is a JavaScript module named in the `plugin` array of opencode's config.
// That module runs inside the agent's own Bun process, so it can do the one
// thing a shell hook cannot — hold a permission open, ask helios, and answer
// opencode out of band.
//
// Three things are measured against opencode 1.18.16 and are the reason this
// file looks the way it does:
//
//   - The `permission.ask` plugin hook does not exist any more. The name
//     survives in opencode's own plugin documentation and in the shipped
//     TypeScript definitions, but the string is nowhere in the runtime:
//     permissions are published on the event bus as `permission.asked` and
//     answered by POSTing the decision to the session's permission endpoint
//     via client.postSessionIdPermissionsPermissionId(); there is no
//     client.permission namespace on the plugin client. Mutating an
//     `output.status` here would be answering a hook that never fires.
//   - Subagents are sessions. Every spawned agent gets its own opencode
//     session that is created, goes busy, goes idle and is deleted just like a
//     real one. They carry `info.parentID`, and everything below filters on it
//     — without that a session with five subagents reports six sessions and
//     six "turn finished" cards.
//   - The event bus is awaited. `trigger` waits for this module's `event`
//     handler, so anything slow in it stalls opencode's own event delivery.
//     The permission path therefore does not await: it hands off to a floating
//     promise and returns immediately. The agent stays blocked on the
//     permission, which is correct, and the bus does not.

import { appendFileSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";

const ENDPOINT = "__HELIOS_ENDPOINT__";
const TRANSCRIPT_DIR = "__HELIOS_TRANSCRIPT_DIR__";
const VERSION = "__HELIOS_VERSION__";

// How long a permission may wait for a human on some surface.
//
// Deliberately longer than the daemon's own wait, for the reason codex's hook
// timeout is: helios has to give up first. If this expired first, opencode
// would be answered by a plugin that had stopped listening while a card was
// still live on the phone, and the two would disagree about what happened.
//
// Wrapped in Number() so this file is valid JavaScript before substitution and
// can be linted and read as written.
const DECISION_TIMEOUT_MS = Number("__HELIOS_DECISION_TIMEOUT_MS__");

// Everything else is fire-and-forget narration. A daemon that is down must
// cost the agent no more than this.
const NOTIFY_TIMEOUT_MS = 5000;

const debug = process?.env?.HELIOS_PLUGIN_DEBUG === "1";
const log = (...args) => {
  if (debug) console.error("[helios]", ...args);
};

export const HeliosPlugin = async ({ client, directory }) => {
  // opencode may load this module twice — once from the `plugin` array and
  // once from a plugin directory it decided to scan. Two copies would double
  // every event and race two replies onto one permission.
  if (globalThis.__heliosOpencodePlugin) return {};
  globalThis.__heliosOpencodePlugin = VERSION;

  // The id helios minted, when helios started this agent. opencode mints its
  // own and helios has never seen it, so this is what lets a hook name a row
  // the daemon already has. Absent for a session the user started by hand,
  // where opencode's id becomes the row key instead — the same two-branch rule
  // the daemon applies in sessionKey().
  const heliosSession = process?.env?.HELIOS_SESSION || "";
  const cwd = directory || process?.cwd?.() || "";

  // Root sessions only. A missing entry means "not yet known", which is not
  // the same as "not a child" — see isChild.
  const parentage = new Map();
  // Per-session busy/idle, so a turn reports one stop rather than one per
  // status event.
  const busy = new Set();

  const key = (sessionID) => heliosSession || sessionID || "";

  const post = async (route, body, { timeout = NOTIFY_TIMEOUT_MS } = {}) => {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeout);
    try {
      const headers = { "Content-Type": "application/json" };
      if (heliosSession) headers["X-Helios-Session"] = heliosSession;
      const res = await fetch(`${ENDPOINT}/${route}`, {
        method: "POST",
        headers,
        body: JSON.stringify(body),
        signal: controller.signal,
      });
      if (!res.ok) {
        log(route, "answered", res.status);
        return null;
      }
      return await res.json().catch(() => ({}));
    } catch (err) {
      // Best effort, always. A daemon that is not running, a port that moved,
      // a laptop that slept — none of them are the agent's problem.
      log(route, "failed:", err?.message || err);
      return null;
    } finally {
      clearTimeout(timer);
    }
  };

  const notify = (route, body) => {
    void post(route, body).catch(() => {});
  };

  // ==================== Root-session filtering ====================

  // isChild answers from the cache when it can and asks opencode when it
  // cannot.
  //
  // On an unresolvable lookup the answer is `true`. That is the safe
  // direction: mistaking the root for a child loses one event, while mistaking
  // a child for the root invents a session helios will show, title, and offer
  // to resume.
  const isChild = async (sessionID) => {
    if (!sessionID) return true;
    if (parentage.has(sessionID)) return parentage.get(sessionID);
    try {
      const sessions = await client.session.list();
      const found = (sessions?.data ?? []).find((s) => s.id === sessionID);
      if (!found) return true;
      const child = Boolean(found.parentID);
      parentage.set(sessionID, child);
      return child;
    } catch (err) {
      log("session lookup failed:", err?.message || err);
      return true;
    }
  };

  const rootOnly = async (sessionID, fn) => {
    if (await isChild(sessionID)) return;
    await fn();
  };

  // ==================== Transcript ====================

  // opencode keeps its conversation in a SQLite database, which cannot be read
  // a record at a time and whose schema is its own business. helios reads
  // transcripts line by line, so the plugin writes the one it can: an
  // append-only file of exactly the records opencode already hands it.
  //
  // Named by the daemon's own session key so the Go side can find it from a
  // session id alone, and written only for root sessions so a subagent's
  // fifteen tool calls do not bury the conversation the user had.
  const transcriptPath = (sessionID) => {
    const id = key(sessionID);
    if (!TRANSCRIPT_DIR || !id) return "";
    return join(TRANSCRIPT_DIR, `${id}.jsonl`);
  };

  const record = (sessionID, entry) => {
    const path = transcriptPath(sessionID);
    if (!path) return;
    try {
      mkdirSync(dirname(path), { recursive: true });
      appendFileSync(
        path,
        JSON.stringify({ ts: new Date().toISOString(), ...entry }) + "\n",
      );
    } catch (err) {
      log("transcript write failed:", err?.message || err);
    }
  };

  // ==================== Summaries ====================

  const truncate = (s, n) =>
    typeof s === "string" && s.length > n ? s.slice(0, n) + "..." : s || "";

  // summarize picks the one field of a tool call worth reading in a list. bash
  // puts it in `command`, the file tools in a path; anything else is shown as
  // the JSON it is, cut short.
  const summarize = (args) => {
    if (!args || typeof args !== "object") {
      return truncate(String(args ?? ""), 100);
    }
    for (const field of ["command", "filePath", "path", "pattern", "query", "url"]) {
      if (typeof args[field] === "string" && args[field]) {
        return truncate(args[field], 100);
      }
    }
    try {
      return truncate(JSON.stringify(args), 100);
    } catch {
      return "";
    }
  };

  // ==================== Permission ====================

  // askHelios raises a card, waits for whoever answers it, and tells opencode.
  //
  // Never awaited by the caller. opencode awaits this module's event handler,
  // so awaiting a five-minute decision here would stop the bus delivering
  // anything else for five minutes — including the `permission.replied` that
  // says a human at the terminal already dealt with it.
  //
  // No reply at all is a deliberate outcome. When helios cannot be reached the
  // permission is left exactly as opencode raised it, and the person sitting
  // at the terminal answers it as they always could. Replying "reject" on a
  // daemon that is merely absent would turn a missing phone into a denied tool
  // call.
  const askHelios = async (props) => {
    const sessionID = props?.sessionID;
    if (!sessionID || !props?.id) return;
    if (await isChild(sessionID)) return;

    const answer = await post(
      "permission",
      {
        session_id: sessionID,
        cwd,
        request_id: props.id,
        tool_name: props.permission || "",
        patterns: props.patterns || [],
        call_id: props.tool?.callID || "",
        metadata: props.metadata || {},
      },
      { timeout: DECISION_TIMEOUT_MS },
    );
    if (!answer || !answer.decision) return;

    // "always" is opencode's own rule-writing reply and is why this provider
    // can offer a choice codex cannot. It is only sent when the person asked
    // for it: an approval that silently stopped asking would be a surprise.
    let reply = "reject";
    if (answer.decision === "allow") {
      reply = answer.scope === "always" ? "always" : "once";
    }

    try {
      await client.postSessionIdPermissionsPermissionId({
        path: { id: sessionID, permissionID: props.id },
        query: { directory: cwd },
        body: { response: reply },
      });
      log("permission", props.id, "->", reply);
    } catch (err) {
      // The usual cause is that the terminal answered first and the request is
      // already gone. Nothing to repair.
      log("permission reply failed:", err?.message || err);
    }
  };

  // ==================== Hooks ====================

  return {
    event: async ({ event }) => {
      const type = event?.type;
      const props = event?.properties;
      if (!type || !props) return;

      switch (type) {
        case "session.created": {
          const info = props.info;
          if (!info?.id) return;
          const child = Boolean(info.parentID);
          // Cached eagerly, because session.deleted fires after the session is
          // gone from list() and the lookup would then fail closed.
          parentage.set(info.id, child);
          if (child) return;
          notify("session/start", {
            session_id: info.id,
            cwd: info.directory || cwd,
            transcript_path: transcriptPath(info.id),
            model: info.model?.modelID || "",
            agent: info.agent || "",
          });
          return;
        }

        case "session.deleted": {
          const info = props.info;
          if (!info?.id) return;
          const known = parentage.get(info.id);
          parentage.delete(info.id);
          busy.delete(info.id);
          if (known !== false) return;
          notify("session/end", {
            session_id: info.id,
            cwd: info.directory || cwd,
          });
          return;
        }

        case "permission.asked":
          void askHelios(props).catch((err) => log("permission failed:", err));
          return;

        case "session.status": {
          const sessionID = props.sessionID;
          const status = props.status?.type;
          if (!sessionID) return;
          if (status === "busy") {
            await rootOnly(sessionID, async () => {
              busy.add(sessionID);
            });
            return;
          }
          if (status !== "idle") return;
          // Only the busy→idle edge. opencode republishes idle, and a card per
          // republish is how a finished turn came to notify three times.
          if (!busy.delete(sessionID)) return;
          await rootOnly(sessionID, async () => {
            notify("stop", {
              session_id: sessionID,
              cwd,
              transcript_path: transcriptPath(sessionID),
            });
          });
          return;
        }

        case "session.error": {
          const sessionID = props.sessionID;
          if (!sessionID || !busy.delete(sessionID)) return;
          await rootOnly(sessionID, async () => {
            notify("stop", {
              session_id: sessionID,
              cwd,
              transcript_path: transcriptPath(sessionID),
              detail: String(props.error?.name || "error"),
            });
          });
          return;
        }

        default:
          return;
      }
    },

    "chat.message": async (input, output) => {
      const sessionID = input?.sessionID;
      if (!sessionID) return;
      await rootOnly(sessionID, async () => {
        const text = (output?.parts ?? [])
          .filter((p) => p?.type === "text" && p.text)
          .map((p) => p.text)
          .join("\n")
          .trim();
        record(sessionID, { role: "user", content: text });
        notify("prompt/submit", {
          session_id: sessionID,
          cwd,
          transcript_path: transcriptPath(sessionID),
          prompt: text,
          model: input?.model?.modelID || "",
          agent: input?.agent || "",
        });
      });
    },

    "tool.execute.before": async (input, output) => {
      const sessionID = input?.sessionID;
      if (!sessionID) return;
      await rootOnly(sessionID, async () => {
        record(sessionID, {
          role: "tool_use",
          tool: input.tool || "",
          summary: summarize(output?.args),
        });
        notify("tool/pre", {
          session_id: sessionID,
          cwd,
          tool_name: input.tool || "",
          tool_input: output?.args ?? null,
          call_id: input.callID || "",
        });
      });
    },

    "tool.execute.after": async (input, output) => {
      const sessionID = input?.sessionID;
      if (!sessionID) return;
      await rootOnly(sessionID, async () => {
        // Capped hard. A single read can return a whole file, and the
        // transcript is a history panel rather than an archive of tool output.
        record(sessionID, {
          role: "tool_result",
          tool: input.tool || "",
          summary: truncate(output?.output ?? output?.title ?? "", 2000),
        });
        notify("tool/post", {
          session_id: sessionID,
          cwd,
          tool_name: input.tool || "",
          tool_input: input.args ?? null,
          call_id: input.callID || "",
        });
      });
    },

    // The only place a completed assistant message is offered to a plugin.
    // Marked experimental by opencode; it is read-only here, so the worst a
    // rename costs is a transcript with no assistant turns in it.
    "experimental.text.complete": async (input, output) => {
      const sessionID = input?.sessionID;
      const text = (output?.text || "").trim();
      if (!sessionID || !text) return;
      await rootOnly(sessionID, async () => {
        record(sessionID, { role: "assistant", content: text });
      });
    },
  };
};
