package opencode

import (
	"encoding/json"
	"fmt"

	"github.com/kamrul1157024/helios/internal/notifications"
	"github.com/kamrul1157024/helios/internal/provider"
	"github.com/kamrul1157024/helios/internal/store"
)

// ActionRoutes carries the clients' labels alongside the handlers, so the
// notification catalogue is served rather than hardcoded once per client.
//
// Two types, where Claude has six. opencode has no AskUserQuestion tool and no
// elicitation event, so there is no question card and no elicitation card, and
// it reports no failed turn, so there is no error-retry card. Absent is the
// honest representation: a card nobody can answer is worse than no card.
//
// There is no trust card either, and its absence is a real difference rather
// than a gap. Claude and Codex both open on a directory-trust dialog that no
// hook reports, which is why they need a card answered by keystroke; opencode
// opens straight into the TUI and asks nothing.
func (p *Provider) ActionRoutes() map[string]provider.ActionRoute {
	return map[string]provider.ActionRoute{
		"opencode.permission": {
			Handler:  handlePermissionAction,
			Label:    "Permission requests",
			Detail:   "opencode is asking to run something that needs your approval.",
			Blocking: true, Group: "action_required", DefaultAlert: true,
		},
		"opencode.done": {
			Handler:  handleDoneAction,
			Label:    "Turn completed",
			Detail:   "opencode finished a turn.",
			Blocking: false, Group: "info", DefaultAlert: true,
		},
	}
}

// handlePermissionAction turns a phone answer into a decision the hook handler
// can act on.
//
// Three actions, not two. opencode's own reply vocabulary is once/always/reject,
// so "approve and stop asking" is something the agent can genuinely be told —
// unlike Codex, whose PermissionRequest cannot write a rule and where the same
// button would be a lie. The scope rides in the decision's Response rather
// than in its Status, because Status is the daemon's own vocabulary and every
// surface reads it.
//
// "approve" and "approve_always" are separate actions rather than one action
// with a flag so that a client which knows nothing about scopes cannot
// accidentally send the wider one.
func handlePermissionAction(notif *store.Notification, body json.RawMessage) (notifications.Decision, error) {
	var req struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return notifications.Decision{}, fmt.Errorf("invalid body: %w", err)
	}
	switch req.Action {
	case "approve":
		return notifications.Decision{Status: "approved"}, nil
	case "approve_always":
		return notifications.Decision{
			Status:   "approved",
			Response: json.RawMessage(`{"scope":"always"}`),
		}, nil
	case "deny":
		return notifications.Decision{Status: "denied"}, nil
	default:
		return notifications.Decision{}, fmt.Errorf("action must be approve/approve_always/deny")
	}
}

// handleDoneAction dismisses a completion notice. It exists so the type has a
// route and therefore a catalogue entry; there is nothing to decide.
func handleDoneAction(notif *store.Notification, body json.RawMessage) (notifications.Decision, error) {
	return notifications.Decision{Status: "dismissed"}, nil
}
