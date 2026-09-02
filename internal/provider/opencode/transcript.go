package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/kamrul1157024/helios/internal/transcript"
)

// maxLineBytes bounds one transcript entry. A tool result can hold an entire
// file, so the cap is generous; what matters is that there is one.
const maxLineBytes = 16 << 20

// opencode keeps no transcript helios can read.
//
// Claude and Codex both append JSON records to a file per session, so their
// readers open that file and parse what is new. opencode does not: the
// conversation lives in one SQLite database, `~/.local/share/opencode/opencode.db`,
// shared by every session on the machine and holding the sessions of every
// project at once.
//
// Three reasons that is not a transcript source:
//
//   - The schema is internal and undocumented. Reading it makes helios depend
//     on a private table layout that a patch release may change, and the
//     failure would be a session that silently stops having a history.
//   - It cannot be read incrementally. The store's whole design is "parse the
//     bytes appended since last time", and a database is not append-only from
//     the outside — the only honest read is the whole session, every poll.
//   - The agent holds it open. It is one writer's database, and helios has no
//     business being a second reader of a file that is mid-transaction while
//     an agent is running.
//
// So the plugin writes the transcript instead. It sees every user message,
// every tool call and result, and every completed assistant message, and it
// appends them as JSON lines in exactly the format below. The file is ours,
// the format is ours, and nothing here depends on opencode's internals.

// entry is one line the plugin appended. The four roles are the four things a
// plugin hook can observe; see plugin/helios.js.
type entry struct {
	Timestamp string `json:"ts"`
	Role      string `json:"role"` // user | assistant | tool_use | tool_result
	Content   string `json:"content,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Summary   string `json:"summary,omitempty"`
}

// ParseLine turns one appended line into the message it holds.
//
// Always zero or one message, never more, because the plugin writes one record
// per event. That makes the seq arithmetic trivial and is worth the extra
// lines in the plugin.
func ParseLine(line []byte, seq int) []transcript.Message {
	var e entry
	if json.Unmarshal(line, &e) != nil {
		return nil
	}
	msg := transcript.Message{Seq: seq, Timestamp: e.Timestamp}
	switch e.Role {
	case "user":
		msg.Role = transcript.RoleUser
		msg.Content = e.Content
	case "assistant":
		msg.Role = transcript.RoleAssistant
		msg.Content = e.Content
	case "tool_use":
		msg.Role = transcript.RoleToolUse
		msg.Tool = e.Tool
		msg.Content = e.Summary
	case "tool_result":
		msg.Role = transcript.RoleToolResult
		msg.Tool = e.Tool
		msg.Content = e.Summary
	default:
		return nil
	}
	if msg.Content == "" && msg.Tool == "" {
		return nil
	}
	return []transcript.Message{msg}
}

func (p *Provider) ParseLine(line []byte, seq int) []transcript.Message {
	return ParseLine(line, seq)
}

// TranscriptDir is where the plugin writes its transcripts.
//
// Under helios's own directory rather than opencode's, because these files are
// helios's: opencode does not know they exist and would not miss them. Keeping
// them out of the agent's data directory means uninstalling helios takes its
// own droppings with it.
func TranscriptDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".helios", "transcripts", "opencode"), nil
}

// LocateTranscript finds a session's transcript when the recorded path has
// gone stale.
//
// One stat, because the plugin names the file after the session key rather
// than after anything about the machine it ran on — so unlike Claude's, whose
// path encodes the cwd and moves when a worktree does, the answer here is
// either at the one path it could be at or nowhere.
func LocateTranscript(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	dir, err := TranscriptDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

func (p *Provider) LocateTranscript(sessionID string) string {
	return LocateTranscript(sessionID)
}
