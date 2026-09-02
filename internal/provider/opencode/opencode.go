// Package opencode drives the opencode CLI.
//
// opencode is the odd one of the three. Claude and Codex both read a hook
// table off disk and shell out to it; opencode has no hook table at all. Its
// only extension point is a JavaScript module named in its config, loaded into
// the agent's own Bun process. So this package ships a plugin (plugin/helios.js,
// embedded), installs it, and serves the same POST routes the other two
// providers serve.
//
// Everything below is measured against opencode 1.18.16. Four findings shape
// the design:
//
//   - The `permission.ask` plugin hook is documented and typed but not
//     implemented: the literal does not appear in the runtime. Permissions
//     arrive as the `permission.asked` bus event and are answered out of band
//     with client.permission.reply(). That is what makes a phone approval
//     possible, and it is also why this provider can offer "allow always" —
//     opencode's reply vocabulary is once/always/reject, where Codex's is only
//     allow/deny.
//   - Subagents are full sessions with their own lifecycle. The plugin filters
//     them on info.parentID; without that a session with five subagents would
//     report six.
//   - There is no --session-id. opencode mints its own id and reports it
//     through the plugin, exactly as Codex does, so HeliosSessionEnv carries
//     ours into the agent's environment and the plugin sends it back.
//   - The conversation lives in a SQLite database, not an append-only log.
//     See transcript.go for what that costs and what is done about it.
package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/kamrul1157024/helios/internal/backend"
	"github.com/kamrul1157024/helios/internal/provider"
	"github.com/kamrul1157024/helios/internal/store"
)

// Provider drives the opencode CLI.
type Provider struct {
	bin  string
	port int
}

// New returns the opencode provider. internalPort is the daemon's own port,
// which the installed plugin posts to.
func New(internalPort int) *Provider {
	return &Provider{bin: findOpencode(), port: internalPort}
}

func (p *Provider) Info() provider.Info {
	return provider.Info{
		ID:      "opencode",
		Name:    "opencode",
		Icon:    "terminal",
		Kind:    provider.KindNative,
		Command: "opencode",
	}
}

func findOpencode() string {
	path, _ := provider.LookAgent("opencode")
	return path
}

// Available reports whether the opencode CLI is on this machine.
//
// Resolved on every call rather than cached at construction, for the reason
// Codex's is: the daemon may start before the user installs the agent, and a
// stale "not installed" would outlive the install.
func (p *Provider) Available() bool {
	_, found := provider.LookAgent("opencode")
	return found
}

// ==================== Permission modes ====================

// opencode has two positions, not a scale.
//
// Its permission rules live in config — per agent, per tool, per pattern — and
// the CLI exposes exactly one flag over them: --auto. So the honest set is the
// two things a launch can actually choose between, and anything finer is the
// user's config to write rather than something helios can offer in a picker.
const (
	// ModeAsk leaves opencode's own permission rules in force, which is what
	// makes the phone useful: a tool that would ask a human raises a card.
	ModeAsk = "ask"
	// ModeAuto is --auto, which approves anything not explicitly denied.
	ModeAuto = "auto"
)

// DefaultPermissionMode is what a helios-started opencode session runs under.
//
// Same reasoning as Codex's: --auto answers every permission before the plugin
// can raise a card, so a session started that way is invisible from the phone
// in the one way that matters. Ask is the mode that keeps helios useful.
const DefaultPermissionMode = ModeAsk

var permissionModes = []string{ModeAsk, ModeAuto}

func (p *Provider) PermissionModes() []string { return permissionModes }

func (p *Provider) ValidMode(mode string) bool { return slices.Contains(permissionModes, mode) }

func modeArgs(mode string) []string {
	if mode == ModeAuto {
		return []string{"--auto"}
	}
	return nil
}

// launchMode reports the mode a spec resolves to, so the daemon records what
// the session is actually running in rather than inferring it later.
func launchMode(spec provider.SessionSpec) string {
	switch {
	case spec.SkipPermissions:
		return ModeAuto
	case slices.Contains(permissionModes, spec.PermissionMode):
		return spec.PermissionMode
	default:
		return DefaultPermissionMode
	}
}

// ==================== Launch ====================

// HeliosSessionEnv carries helios's own session id into the agent's
// environment.
//
// opencode has no --session-id for a new session: --session names one that
// already exists. The plugin inherits this variable from the agent's process
// and sends it back as a header, which is the only way a hook can name a
// session row helios minted before opencode had an id of its own.
const HeliosSessionEnv = "HELIOS_SESSION"

// Launch starts a fresh opencode TUI.
//
// The prompt goes in --prompt rather than positionally: the positional
// argument to `opencode` is the *project directory*, so a prompt written there
// is read as a path and the agent starts in a directory that does not exist.
// That is the trap this flag exists to avoid, and it is the opposite of Codex,
// where the prompt is positional and must come last.
func (p *Provider) Launch(spec provider.SessionSpec) (provider.Launch, error) {
	mode := launchMode(spec)
	argv := []string{p.bin}
	argv = append(argv, modeArgs(mode)...)
	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}
	if spec.Prompt != "" {
		argv = append(argv, "--prompt", spec.Prompt)
	}
	return provider.Launch{
		Argv: argv,
		Env:  map[string]string{HeliosSessionEnv: spec.SessionID},
		Mode: mode,
	}, nil
}

// Resume wakes a session by the id opencode minted, which is what resumeID
// holds.
//
// An empty resumeID means the plugin never reported in, so there is nothing to
// resume by. Returning empty argv is the honest answer: the caller falls back
// rather than starting a fresh conversation that pretends to be the old one.
func (p *Provider) Resume(sessionID, resumeID, mode string) (provider.Launch, error) {
	if resumeID == "" {
		return provider.Launch{}, nil
	}
	if !slices.Contains(permissionModes, mode) {
		mode = DefaultPermissionMode
	}
	argv := []string{p.bin}
	argv = append(argv, modeArgs(mode)...)
	argv = append(argv, "--session", resumeID)
	return provider.Launch{
		Argv: argv,
		Env:  map[string]string{HeliosSessionEnv: sessionID},
		Mode: mode,
	}, nil
}

// ==================== Models ====================

// Models asks opencode which models this machine is actually authenticated
// for, rather than shipping a list that goes stale.
//
// `opencode models` prints one "provider/model" per line. --pure keeps the
// user's own plugins out of it: without it, a plugin that logs on load writes
// its banner to the same stdout and the first "model" in the list is a log
// line.
func (p *Provider) Models() ([]provider.ModelInfo, error) {
	out, err := exec.Command(p.bin, "models", "--pure").Output()
	if err != nil {
		return nil, fmt.Errorf("opencode models: %w", err)
	}
	return parseModels(string(out)), nil
}

func parseModels(out string) []provider.ModelInfo {
	models := []provider.ModelInfo{}
	scanner := bufio.NewScanner(strings.NewReader(out))
	seen := map[string]bool{}
	for scanner.Scan() {
		id := strings.TrimSpace(scanner.Text())
		// A model id is "provider/model". Anything else on this stream is a
		// banner or a warning, and listing it would offer the user a model
		// that cannot be launched.
		vendor, name, ok := strings.Cut(id, "/")
		if !ok || vendor == "" || name == "" || strings.ContainsAny(id, " \t") {
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, provider.ModelInfo{ID: id, Name: name, Description: vendor})
	}
	return models
}

func (p *Provider) Commands() []provider.Command {
	return []provider.Command{
		{Name: "/new", Description: "Start a new session", Icon: "add"},
		{Name: "/sessions", Description: "Switch session", Icon: "list"},
		{Name: "/models", Description: "Switch model", Icon: "swap_horiz"},
		{Name: "/agent", Description: "Switch agent", Icon: "smart_toy"},
		{Name: "/compact", Description: "Compact conversation context", Icon: "compress"},
		{Name: "/undo", Description: "Revert the last change", Icon: "undo"},
		{Name: "/share", Description: "Share the session", Icon: "share"},
		{Name: "/exit", Description: "Quit opencode", Icon: "logout"},
	}
}

func (p *Provider) EventTypes() []provider.EventTypeInfo {
	return []provider.EventTypeInfo{
		{Type: "tool_pre", Label: "Tool Started", Description: "A tool is about to run", Category: "tools"},
		{Type: "tool_post", Label: "Tool Completed", Description: "A tool finished", Category: "tools"},
		{Type: "prompt_submit", Label: "Prompt Submitted", Description: "User sent a new prompt", Category: "actions"},
		{Type: "permission", Label: "Permission Needed", Description: "Waiting for approval", Category: "actions"},
		{Type: "stop", Label: "Turn Finished", Description: "The agent finished a turn", Category: "lifecycle"},
		{Type: "session_start", Label: "Session Started", Description: "A new session began", Category: "lifecycle"},
		{Type: "session_end", Label: "Session Ended", Description: "Session was closed", Category: "lifecycle"},
	}
}

// ==================== Prompt queue ====================

// terminalBackend is set by the daemon once shared state exists.
var terminalBackend backend.Backend

// SetBackend gives the action handlers access to session terminals.
func SetBackend(b backend.Backend) { terminalBackend = b }

func (p *Provider) SetBackend(b backend.Backend) { SetBackend(b) }

// QueuePrompt types the prompt into the session's terminal, which opencode
// holds until the current turn ends — the same behaviour Claude and Codex
// have.
//
// The alternative is the HTTP API the running agent already serves, which
// would deliver a prompt without a terminal at all. It is not used: reaching
// it needs the port opencode chose at start-up, which is picked at random
// unless --port was passed and is reported nowhere helios can read. The PTY
// path needs nothing and already works.
func (p *Provider) QueuePrompt(sessionID, resumeID, text string) error {
	if terminalBackend == nil {
		return errNoTerminal
	}
	return terminalBackend.SendText(sessionID, text)
}

// ==================== Small model and titles ====================

// Complete runs the CLI non-interactively, which respects whatever auth the
// user already has rather than asking for a key.
//
// Three flags carry the weight:
//
//   - --pure keeps the user's plugins out, including this one. That is what
//     makes a one-shot invisible: no plugin means no session/start, so helios
//     never sees the run it made to name a session. Codex had to detect and
//     discard those after the fact; here they simply do not happen.
//   - --format json turns the reply into events with the text in a field,
//     instead of a rendered transcript that has to be scraped back apart.
//   - No --model, so opencode uses whatever the user configured. Naming a
//     model here would be a guess about an account helios cannot see.
func (p *Provider) Complete(ctx context.Context, system, prompt string) (string, error) {
	cmd := exec.CommandContext(ctx, p.bin,
		"run", "--pure", "--format", "json",
		system+"\n\n"+prompt,
	)
	// A session's own directory is irrelevant to naming it, and running in one
	// pulls that project's AGENTS.md into the context of a request that only
	// needs to write eight words.
	cmd.Dir = os.TempDir()

	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return "", fmt.Errorf("opencode run: %w: %s", err, truncate(strings.TrimSpace(string(exit.Stderr)), 200))
		}
		return "", fmt.Errorf("opencode run: %w", err)
	}
	return textFromRunJSON(out), nil
}

// runEvent is one line of `opencode run --format json`.
type runEvent struct {
	Type string `json:"type"`
	Part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"part"`
}

// textFromRunJSON pulls the assistant's answer out of the event stream.
//
// Only "text" events, so the step_start and step_finish bookkeeping — and the
// token counts and provider metadata riding on them — never reach the titler.
// This is the whole reason --format json is worth the flag: Codex has to guess
// which line of rendered output was the answer, and this does not.
func textFromRunJSON(out []byte) string {
	var parts []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for scanner.Scan() {
		var ev runEvent
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			continue
		}
		if ev.Type != "text" || ev.Part.Text == "" {
			continue
		}
		parts = append(parts, ev.Part.Text)
	}
	return strings.TrimSpace(strings.Join(parts, ""))
}

// Title and AutoTitle delegate to the shared implementation, which reads the
// transcript through this provider's parser and narrates with Complete above.
func (p *Provider) Title(db *store.Store, sessionID, cwd, transcriptPath string, notify provider.Notify) string {
	return provider.RegenerateTitle(db, sessionID, cwd, transcriptPath, notify)
}

func (p *Provider) AutoTitle(ctx *provider.HookContext, sessionID, cwd, transcriptPath string, notify provider.Notify) {
	provider.TriggerAutoTitle(ctx, sessionID, cwd, transcriptPath, notify)
}

// ==================== Capabilities deliberately absent ====================
//
// ScreenWatcher. Claude and Codex each block on a directory-trust dialog that
// no hook reports, which is the entire reason the interface exists. opencode
// asks nothing at start-up — it opens straight into the TUI — so there is no
// screen to match and a matcher here could only produce false positives.
//
// Discoverer. Finding sessions the user started outside helios means reading
// opencode's store, and that store is one SQLite database whose schema is
// internal and undocumented. Walking it would couple helios to a private
// schema for sessions it could not show a transcript of anyway; see
// transcript.go.
