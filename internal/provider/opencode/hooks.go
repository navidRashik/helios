package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"time"

	"github.com/kamrul1157024/helios/internal/backend"
	"github.com/kamrul1157024/helios/internal/hitl"
	"github.com/kamrul1157024/helios/internal/notifications"
	"github.com/kamrul1157024/helios/internal/provider"
	"github.com/kamrul1157024/helios/internal/store"
)

var errNoTerminal = errors.New("opencode: session has no live terminal")

// hookInput is the payload the plugin sends.
//
// Both ends of this are ours — plugin/helios.js writes it, this reads it — so
// unlike the Claude and Codex payloads it is a contract rather than a
// measurement. It is shaped like theirs on purpose: a reader who knows one
// knows all three.
type hookInput struct {
	// SessionID is the id opencode minted, which is the only thing
	// `opencode --session` accepts.
	SessionID      string `json:"session_id"`
	CWD            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	Model          string `json:"model,omitempty"`
	Agent          string `json:"agent,omitempty"`

	Prompt string `json:"prompt,omitempty"`

	ToolName  string          `json:"tool_name,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	CallID    string          `json:"call_id,omitempty"`

	// RequestID is opencode's own permission id, and the thing
	// client.permission.reply() is keyed on.
	RequestID string   `json:"request_id,omitempty"`
	Patterns  []string `json:"patterns,omitempty"`

	Detail string `json:"detail,omitempty"`
}

// heliosSessionHeader carries the id helios minted.
//
// opencode mints its own session id and helios has never seen it, so without
// this header a hook cannot be matched to a session row. The launch
// environment puts HELIOS_SESSION in the agent's process and the plugin sends
// it back. See HeliosSessionEnv.
const heliosSessionHeader = "X-Helios-Session"

// sessionKey is the row a hook belongs to.
//
// Three answers, in order, and the same three Codex uses. The header, when
// helios launched the session. Then a row already bound to this opencode id,
// which covers a hook that lost the environment and which without this lookup
// would mint a second, ghost row for a session already being tracked. Then the
// opencode id itself, which is a session the user started by hand and is how
// those become visible for free.
func sessionKey(ctx *provider.HookContext, r *http.Request, in *hookInput) string {
	if id := r.Header.Get(heliosSessionHeader); id != "" {
		return id
	}
	if sess, err := ctx.DB.SessionByResumeID(in.SessionID); err == nil && sess != nil {
		return sess.SessionID
	}
	return in.SessionID
}

// decisionTimeout is how long a blocking hook waits for a human on some
// surface.
const decisionTimeout = 5 * time.Minute

// PluginDecisionTimeout is what the plugin is told to wait. It clears
// decisionTimeout by a margin on purpose: helios has to give up first, or the
// plugin abandons a request while a card is still live on the phone and the
// two race to decide what happened.
const PluginDecisionTimeout = decisionTimeout + 30*time.Second

func (p *Provider) HookRoutes() map[string]provider.HookHandler {
	return map[string]provider.HookHandler{
		"session/start": handleSessionStart,
		"session/end":   handleSessionEnd,
		"prompt/submit": handlePromptSubmit,
		"tool/pre":      handleToolPre,
		"tool/post":     handleToolPost,
		"permission":    handlePermission,
		"stop":          handleStop,
	}
}

// Routes opencode has no source for, and why they are absent rather than
// registered and silent:
//
//	compact/pre, compact/post   opencode compacts without publishing an event a
//	                            plugin can see; experimental.session.compacting
//	                            fires only when a plugin rewrites the prompt.
//	subagent/start, /stop       a subagent is a full session here, not a nested
//	                            event, and the plugin filters those out on
//	                            parentID so they never reach the daemon as
//	                            sessions of their own.
//
// A route with nothing to call it is a URL that 404s in the logs and a
// capability the health check cannot tell from a broken one.

// decode reads a hook body, or writes the error and reports failure.
func decode(w http.ResponseWriter, raw json.RawMessage) (*hookInput, bool) {
	var in hookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return nil, false
	}
	// Evidence that the plugin is loaded and reaching us. Recorded here rather
	// than in the daemon's generic hook entry point because it is only true of
	// this provider's own routes; see install.go.
	noteHookReceived()
	return &in, true
}

func ack(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{}`)
}

// ==================== Lifecycle ====================

func handleSessionStart(ctx *provider.HookContext, w http.ResponseWriter, r *http.Request, raw json.RawMessage) {
	in, ok := decode(w, raw)
	if !ok {
		return
	}
	key := sessionKey(ctx, r, in)

	var transcriptPath *string
	if in.TranscriptPath != "" {
		transcriptPath = &in.TranscriptPath
	}
	var model *string
	if in.Model != "" {
		model = &in.Model
	}

	handle := ""
	if ctx.Terminal != nil {
		handle, _ = ctx.Terminal.Handle(key)
	}
	if ctx.SessionStarted != nil {
		ctx.SessionStarted(key)
	}

	ctx.DB.UpsertSession(&store.Session{
		SessionID:      key,
		Source:         "opencode",
		CWD:            in.CWD,
		TranscriptPath: transcriptPath,
		Model:          model,
		Status:         "idle",
		LastEvent:      strPtr("SessionStart"),
	})

	// The id opencode minted, which is the only thing `opencode --session`
	// accepts. Recorded on every start, not only the first: a resumed session
	// reports the same id, and a new conversation under the same helios row
	// reports a new one.
	if in.SessionID != "" {
		if err := ctx.DB.UpdateSessionResumeID(key, in.SessionID); err != nil {
			log.Printf("opencode: record resume id for %s: %v", key, err)
		}
	}

	renameSessionWindow(ctx, key, "idle", in.CWD)

	data := map[string]interface{}{
		"session_id": key, "cwd": in.CWD, "status": "idle", "model": in.Model,
	}
	if handle != "" {
		data["terminal"] = handle
	}
	ctx.Notify("session_status", data)
	report(ctx, provider.ReportEvent{Type: "session_start", SessionID: key, CWD: in.CWD})
	ack(w)
}

func handleSessionEnd(ctx *provider.HookContext, w http.ResponseWriter, r *http.Request, raw json.RawMessage) {
	in, ok := decode(w, raw)
	if !ok {
		return
	}
	key := sessionKey(ctx, r, in)

	if ctx.Terminal != nil {
		ctx.Terminal.Forget(key)
	}
	// Unless helios stopped the agent itself to reclaim memory. The exit is
	// then ours, not the user's, and the session is cold rather than over.
	// See docs/specs/42-cold-sessions.md.
	if evicter, ok := ctx.Terminal.(backend.Evicter); ok && evicter.EvictedRecently(key) {
		ctx.Notify("session_updated", map[string]interface{}{"session_id": key})
		ack(w)
		return
	}

	ctx.DB.UpdateSessionStatus(key, "terminated", "SessionEnd")
	ctx.Notify("session_status", map[string]interface{}{"session_id": key, "status": "terminated"})
	report(ctx, provider.ReportEvent{Type: "session_end", SessionID: key, CWD: in.CWD})
	ack(w)
}

func handlePromptSubmit(ctx *provider.HookContext, w http.ResponseWriter, r *http.Request, raw json.RawMessage) {
	in, ok := decode(w, raw)
	if !ok {
		return
	}
	key := sessionKey(ctx, r, in)

	ctx.DB.UpdateSessionStatus(key, "active", "UserPromptSubmit")
	updateTranscript(ctx, key, in)
	renameSessionWindow(ctx, key, "active", in.CWD)

	if in.Prompt != "" {
		ctx.DB.UpdateSessionLastUserMessage(key, in.Prompt)
	}
	ctx.Notify("session_status", map[string]interface{}{
		"session_id": key, "status": "active", "last_user_message": in.Prompt,
	})
	report(ctx, provider.ReportEvent{Type: "prompt_submit", SessionID: key, CWD: in.CWD, Message: in.Prompt})

	// Last, so a caller woken by this has the status and the message already
	// written rather than racing them.
	if ctx.PromptSubmitted != nil {
		ctx.PromptSubmitted(key)
	}
	ack(w)
}

func handleStop(ctx *provider.HookContext, w http.ResponseWriter, r *http.Request, raw json.RawMessage) {
	in, ok := decode(w, raw)
	if !ok {
		return
	}
	key := sessionKey(ctx, r, in)

	ctx.DB.UpdateSessionStatus(key, "idle", "Stop")
	updateTranscript(ctx, key, in)
	renameSessionWindow(ctx, key, "idle", in.CWD)

	if _, err := ctx.Mgr.ResolveSession(key, "resolved", "opencode"); err != nil {
		log.Printf("opencode: resolve notifications for %s: %v", key, err)
	}

	sess, _ := ctx.DB.GetSession(key)
	title := "Session completed"
	detail := sessionContext(in.CWD, sess)
	if in.Detail != "" {
		detail = truncate(in.Detail, 200)
	}
	notif := &store.Notification{
		ID:            notifications.GenerateNotificationID(),
		Source:        "opencode",
		SourceSession: key,
		CWD:           in.CWD,
		Type:          "opencode.done",
		Status:        "dismissed",
		Title:         &title,
		Detail:        &detail,
	}
	ctx.Mgr.CreateNotification(notif)
	ctx.Notify("notification", notif)
	ctx.Notify("session_status", map[string]interface{}{
		"session_id": key, "cwd": in.CWD, "status": "idle",
	})
	report(ctx, provider.ReportEvent{Type: "stop", SessionID: key, CWD: in.CWD, Detail: in.Detail})

	if t := provider.TitlerFor("opencode"); t != nil {
		t.AutoTitle(ctx, key, in.CWD, transcriptPathOf(ctx, key, in), ctx.Notify)
	}
	ack(w)
}

// ==================== Tools ====================

func handleToolPre(ctx *provider.HookContext, w http.ResponseWriter, r *http.Request, raw json.RawMessage) {
	in, ok := decode(w, raw)
	if !ok {
		return
	}
	key := sessionKey(ctx, r, in)
	ctx.DB.UpdateSessionStatus(key, "active", "PreToolUse:"+in.ToolName)
	ctx.Notify("session_status", map[string]interface{}{
		"session_id": key, "status": "active", "last_event": "PreToolUse:" + in.ToolName,
	})
	report(ctx, provider.ReportEvent{
		Type: "tool_pre", SessionID: key, CWD: in.CWD,
		ToolName: in.ToolName, ToolInput: summarize(in.ToolInput),
	})
	ack(w)
}

func handleToolPost(ctx *provider.HookContext, w http.ResponseWriter, r *http.Request, raw json.RawMessage) {
	in, ok := decode(w, raw)
	if !ok {
		return
	}
	key := sessionKey(ctx, r, in)
	ctx.DB.UpdateSessionStatus(key, "active", "PostToolUse:"+in.ToolName)
	// A tool ran, so the paths clients are watching may have moved. No path is
	// passed: the daemon compares its own digests and works out what changed.
	// See docs/specs/54-file-change-events.md.
	if ctx.FilesTouched != nil {
		ctx.FilesTouched()
	}
	// The tool ran, so any permission card for it is settled whichever surface
	// answered — most often the terminal, since a person sitting at opencode
	// can always answer its own prompt. Matched by call id when the plugin
	// reported one and by tool name otherwise.
	resolveToolPermissions(ctx, key, in.CallID, in.ToolName)
	report(ctx, provider.ReportEvent{
		Type: "tool_post", SessionID: key, CWD: in.CWD,
		ToolName: in.ToolName, ToolInput: summarize(in.ToolInput),
	})
	ack(w)
}

// ==================== Permission ====================

// permResponse is what the plugin reads back.
//
// Scope is the part Codex has no room for. opencode's reply vocabulary is
// once/always/reject, so an approval can write a rule that stops opencode
// asking again — and that is a different answer from "yes, this once", which
// is why the two are distinct here rather than folded into one boolean.
type permResponse struct {
	Decision string `json:"decision"` // allow | deny
	Scope    string `json:"scope,omitempty"`
	Message  string `json:"message,omitempty"`
}

func writePermResponse(w http.ResponseWriter, decision, scope, message string) {
	w.Header().Set("Content-Type", "application/json")
	resp := permResponse{Decision: decision, Scope: scope, Message: message}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("opencode: encode permission response: %v", err)
	}
}

const (
	allowOnce   = "Allow once"
	allowAlways = "Allow always"
	denyChoice  = "Deny"
)

// scopeAlways marks an approval that should write a rule.
const scopeAlways = "always"

func handlePermission(ctx *provider.HookContext, w http.ResponseWriter, r *http.Request, raw json.RawMessage) {
	in, ok := decode(w, raw)
	if !ok {
		return
	}
	key := sessionKey(ctx, r, in)

	ctx.DB.UpdateSessionStatus(key, "waiting_permission", "PermissionRequest")
	renameSessionWindow(ctx, key, "waiting_permission", in.CWD)

	notifID := notifications.GenerateNotificationID()
	detail := permissionDetail(in)
	payload, _ := json.Marshal(map[string]interface{}{
		"tool_name":  in.ToolName,
		"tool_input": json.RawMessage(in.ToolInput),
		"call_id":    in.CallID,
		"request_id": in.RequestID,
		"patterns":   in.Patterns,
	})
	payloadStr := string(payload)

	notif := &store.Notification{
		ID:            notifID,
		Source:        "opencode",
		SourceSession: key,
		CWD:           in.CWD,
		Type:          "opencode.permission",
		Status:        "pending",
		Title:         &in.ToolName,
		Detail:        &detail,
		Payload:       &payloadStr,
	}
	if err := ctx.Mgr.CreateNotification(notif); err != nil {
		http.Error(w, "failed to create notification", http.StatusInternalServerError)
		return
	}
	// Reserve the decision slot before publishing, so a client that answers
	// immediately cannot beat this handler to WaitForDecision.
	ctx.Mgr.Register(notifID)
	ctx.Notify("notification", notif)
	report(ctx, provider.ReportEvent{
		Type: "permission", SessionID: key, CWD: in.CWD,
		ToolName: in.ToolName, ToolInput: summarize(in.ToolInput),
	})

	// Painted after publishing, so the phone and the terminal race from the
	// same starting line, and taken down once the decision is settled no
	// matter which of them settled it.
	//
	// Three choices, where Codex has two: opencode's reply vocabulary includes
	// "always", so "don't ask again" is a real answer here rather than one the
	// CLI would refuse.
	choices := []string{allowOnce, allowAlways, denyChoice}
	defer showPrompt(ctx, key, hitl.Prompt{
		Title:   in.ToolName,
		Body:    []string{detail},
		Choices: choices,
	}, func(a hitl.Answer) {
		decision := notifications.Decision{Status: "denied"}
		if !a.Cancelled() && a.Index < len(choices) {
			switch choices[a.Index] {
			case allowOnce:
				decision.Status = "approved"
			case allowAlways:
				decision.Status = "approved"
				decision.Response = json.RawMessage(`{"scope":"always"}`)
			}
		}
		if err := ctx.Mgr.Resolve(notifID, decision, "terminal"); err != nil &&
			!errors.Is(err, store.ErrAlreadyResolved) {
			log.Printf("opencode: resolve permission %s from the terminal: %v", notifID, err)
		}
	})()

	decision := waitForDecision(ctx, notifID, r)
	if decision == nil {
		return
	}

	ctx.DB.UpdateSessionStatus(key, "active", "PermissionResolved")
	renameSessionWindow(ctx, key, "active", in.CWD)

	if decision.Status == "approved" {
		writePermResponse(w, "allow", decisionScope(decision), "")
		return
	}
	writePermResponse(w, "deny", "", "Denied via helios")
}

// decisionScope reads the "don't ask again" flag off an approval.
//
// Absent, malformed, or anything other than "always" means once. Failing to
// the narrower answer matters here: the wide one writes a permission rule into
// the user's config that they never see and did not ask for.
func decisionScope(d *notifications.Decision) string {
	if len(d.Response) == 0 {
		return ""
	}
	var body struct {
		Scope string `json:"scope"`
	}
	if json.Unmarshal(d.Response, &body) != nil {
		return ""
	}
	if body.Scope == scopeAlways {
		return scopeAlways
	}
	return ""
}

// permissionDetail is the one line a phone shows about a request.
//
// The tool input is preferred because it is what the person is actually
// deciding on. opencode's patterns are the rule that would be written and are
// the honest fallback when there is no input to show — a bare tool name tells
// nobody anything.
func permissionDetail(in *hookInput) string {
	if summary := summarize(in.ToolInput); summary != "" {
		return fmt.Sprintf("%s: %s", in.ToolName, summary)
	}
	if len(in.Patterns) > 0 {
		return fmt.Sprintf("%s: %s", in.ToolName, truncate(joinPatterns(in.Patterns), 100))
	}
	return in.ToolName
}

func joinPatterns(patterns []string) string {
	out := ""
	for i, p := range patterns {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// ==================== Helpers ====================

func waitForDecision(ctx *provider.HookContext, notifID string, r *http.Request) *notifications.Decision {
	timer := time.NewTimer(decisionTimeout)
	defer timer.Stop()

	ch := make(chan notifications.Decision, 1)
	go func() {
		decision, err := ctx.Mgr.WaitForDecision(notifID)
		if err != nil {
			ch <- notifications.Decision{Status: "denied"}
			return
		}
		ch <- decision
	}()

	select {
	case decision := <-ch:
		return &decision
	case <-timer.C:
		ctx.Mgr.CancelPending(notifID)
		denied := notifications.Decision{Status: "denied"}
		return &denied
	case <-r.Context().Done():
		ctx.Mgr.CancelPendingFromClaude(notifID)
		return nil
	}
}

// showPrompt paints a helios modal over a session's terminal and returns the
// function that takes it down.
//
// A session with no terminal is not a failure: the notification is already on
// the phone. Every caller defers the returned function, so it is always safe
// to call.
func showPrompt(ctx *provider.HookContext, sessionID string, p hitl.Prompt, onAnswer func(hitl.Answer)) func() {
	release, err := ctx.HITL.Ask(sessionID, p, onAnswer)
	if err != nil {
		if !errors.Is(err, hitl.ErrNoTerminal) {
			log.Printf("opencode: show %q prompt for %s: %v", p.Title, sessionID, err)
		}
		return func() {}
	}
	return release
}

// resolveToolPermissions retracts the pending card for a tool that has run.
//
// By call id first, which opencode gives us and Codex does not: it names one
// request exactly, so two concurrent calls to the same tool do not retract
// each other's card. The tool name is the fallback for a permission raised
// before any call id existed.
func resolveToolPermissions(ctx *provider.HookContext, sessionID, callID, toolName string) {
	if callID == "" && toolName == "" {
		return
	}
	pending, err := ctx.Mgr.ListNotifications("opencode", "pending", "opencode.permission")
	if err != nil {
		log.Printf("opencode: list pending permissions for %s: %v", sessionID, err)
		return
	}
	for i := range pending {
		n := &pending[i]
		if n.SourceSession != sessionID {
			continue
		}
		gotCall, gotTool := permissionTarget(n)
		if callID != "" && gotCall != "" {
			if gotCall != callID {
				continue
			}
		} else if gotTool != toolName {
			continue
		}
		ctx.Mgr.CancelPendingFromClaude(n.ID)
	}
}

func permissionTarget(n *store.Notification) (callID, toolName string) {
	if n.Payload == nil {
		return "", ""
	}
	var payload struct {
		CallID   string `json:"call_id"`
		ToolName string `json:"tool_name"`
	}
	if err := json.Unmarshal([]byte(*n.Payload), &payload); err != nil {
		return "", ""
	}
	return payload.CallID, payload.ToolName
}

func updateTranscript(ctx *provider.HookContext, key string, in *hookInput) {
	if in.TranscriptPath != "" {
		ctx.DB.UpdateSessionTranscriptPath(key, in.TranscriptPath)
	}
}

// transcriptPathOf is the path the titler should read.
//
// The plugin reports one on every lifecycle hook, but a session adopted from a
// terminal may have been running before the transcript directory existed, and
// the row then holds the path from an earlier hook. Preferring what the
// session already has keeps a relocated install from renaming a session off
// an empty file.
func transcriptPathOf(ctx *provider.HookContext, key string, in *hookInput) string {
	if in.TranscriptPath != "" {
		return in.TranscriptPath
	}
	if sess, err := ctx.DB.GetSession(key); err == nil && sess != nil && sess.TranscriptPath != nil {
		return *sess.TranscriptPath
	}
	return ""
}

func renameSessionWindow(ctx *provider.HookContext, sessionID, status, cwd string) {
	if ctx.Terminal == nil || !ctx.Terminal.Alive(sessionID) {
		return
	}
	sess, _ := ctx.DB.GetSession(sessionID)
	label := ""
	if sess != nil {
		label = sess.Label(30)
	}
	if err := ctx.Terminal.Rename(sessionID, backend.DisplayName(status, cwd, label)); err != nil {
		log.Printf("opencode: rename terminal for %s: %v", sessionID, err)
	}
}

func report(ctx *provider.HookContext, e provider.ReportEvent) {
	if ctx.Report != nil {
		ctx.Report(e)
	}
}

func summarize(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return truncate(string(raw), 100)
	}
	// The field worth reading, in the order opencode's own tools use them:
	// bash puts it in command, the file tools in a path, grep and glob in a
	// pattern.
	for _, field := range []string{"command", "filePath", "path", "pattern", "query", "url"} {
		if v, ok := m[field].(string); ok && v != "" {
			return truncate(v, 100)
		}
	}
	return truncate(string(raw), 100)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func sessionContext(cwd string, sess *store.Session) string {
	project := filepath.Base(cwd)
	if sess != nil {
		if label := sess.Label(80); label != "" {
			return fmt.Sprintf("%s: %s", project, label)
		}
	}
	return project
}

func strPtr(s string) *string { return &s }
