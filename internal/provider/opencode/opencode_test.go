package opencode

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kamrul1157024/helios/internal/notifications"
	"github.com/kamrul1157024/helios/internal/provider"
	"github.com/kamrul1157024/helios/internal/store"
	"github.com/kamrul1157024/helios/internal/transcript"
)

// ==================== Launch ====================

// --auto answers every permission before the plugin can raise a card, so a
// session started that way is invisible from the phone in the one way that
// matters. Same shape of trap as Codex's full-auto.
func TestLaunchDefaultsToAModeThatKeepsThePhoneUseful(t *testing.T) {
	launch, err := New(7654).Launch(provider.SessionSpec{SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if launch.Mode != ModeAsk {
		t.Errorf("mode = %q, want %q", launch.Mode, ModeAsk)
	}
	if slices.Contains(launch.Argv, "--auto") {
		t.Errorf("argv %q auto-approves; the permission card would never fire", launch.Argv)
	}
}

func TestAutoModeIsPassedThrough(t *testing.T) {
	launch, _ := New(0).Launch(provider.SessionSpec{PermissionMode: ModeAuto})
	if !slices.Contains(launch.Argv, "--auto") {
		t.Errorf("argv %q, want --auto", launch.Argv)
	}
}

func TestSkipPermissionsOverridesTheMode(t *testing.T) {
	launch, _ := New(0).Launch(provider.SessionSpec{
		PermissionMode:  ModeAsk,
		SkipPermissions: true,
	})
	if launch.Mode != ModeAuto {
		t.Errorf("mode = %q, want %q", launch.Mode, ModeAuto)
	}
	if !slices.Contains(launch.Argv, "--auto") {
		t.Errorf("argv %q does not bypass", launch.Argv)
	}
}

// An unknown mode is not an error and is not passed on. Codex's equivalent
// would be rejected by the CLI; opencode's would be a flag that does not
// exist, and the session would fail to start at all.
func TestUnknownModeFallsBackToTheDefault(t *testing.T) {
	launch, _ := New(0).Launch(provider.SessionSpec{PermissionMode: "plan"})
	if launch.Mode != ModeAsk {
		t.Errorf("mode = %q, want %q", launch.Mode, ModeAsk)
	}
	if !New(0).ValidMode(ModeAsk) || New(0).ValidMode("plan") {
		t.Error("ValidMode does not agree with what Launch accepts")
	}
}

// The exact inverse of Codex, and the reason this is a test rather than a
// comment: opencode's positional argument is the *project directory*, so a
// prompt written there is read as a path and the agent starts nowhere.
func TestPromptIsAFlagNotAPositional(t *testing.T) {
	const prompt = `fix the "auth bug" && run $(tests)`
	launch, _ := New(0).Launch(provider.SessionSpec{Prompt: prompt})
	if !hasFlagPair(launch.Argv, "--prompt", prompt) {
		t.Fatalf("argv %q does not carry the prompt as --prompt", launch.Argv)
	}
	if launch.Argv[1] == prompt {
		t.Error("the prompt is in the positional slot, where opencode reads it as a directory")
	}
}

// The environment is the whole answer to opencode having no --session-id: the
// plugin reads it and sends it back as a header, and it is the only way a hook
// can name a session helios already knows about.
func TestLaunchCarriesTheHeliosSessionID(t *testing.T) {
	launch, _ := New(0).Launch(provider.SessionSpec{SessionID: "helios-abc"})
	if launch.Env[HeliosSessionEnv] != "helios-abc" {
		t.Errorf("env[%s] = %q, want helios-abc", HeliosSessionEnv, launch.Env[HeliosSessionEnv])
	}
}

// ==================== Resume ====================

func TestResumeUsesTheIDOpencodeMinted(t *testing.T) {
	launch, err := New(0).Resume("helios-abc", "ses_f9e986f97ffemuES0FlW9jy0US", ModeAsk)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !hasFlagPair(launch.Argv, "--session", "ses_f9e986f97ffemuES0FlW9jy0US") {
		t.Errorf("argv %q does not resume the opencode session", launch.Argv)
	}
	if launch.Env[HeliosSessionEnv] != "helios-abc" {
		t.Error("a resumed session must still identify itself to helios")
	}
}

// A session whose plugin never reported in has no opencode id. Refusing is the
// honest answer: a fresh conversation pretending to be the old one is worse
// than not waking it.
func TestResumeWithoutAnOpencodeIDRefuses(t *testing.T) {
	launch, err := New(0).Resume("helios-abc", "", ModeAsk)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(launch.Argv) != 0 {
		t.Errorf("argv = %q, want none", launch.Argv)
	}
}

// ==================== Models and completion ====================

// `opencode models` shares stdout with anything a plugin prints on load, so
// the parser takes only what is shaped like a model id.
func TestModelParsingIgnoresEverythingThatIsNotAModel(t *testing.T) {
	got := parseModels(strings.Join([]string{
		"[claude-mem] OpenCode plugin loading (project: /tmp)",
		"github-copilot/claude-haiku-4.5",
		"github-copilot/claude-opus-5",
		"github-copilot/claude-haiku-4.5",
		"",
		"warning: something happened",
	}, "\n"))
	if len(got) != 2 {
		t.Fatalf("parsed %d models from a stream with two, got %+v", len(got), got)
	}
	if got[0].ID != "github-copilot/claude-haiku-4.5" {
		t.Errorf("first model = %q", got[0].ID)
	}
	if got[0].Name != "claude-haiku-4.5" || got[0].Description != "github-copilot" {
		t.Errorf("model not split into vendor and name: %+v", got[0])
	}
}

// --format json is worth the flag precisely because the bookkeeping events
// carry token counts that would otherwise end up in a session title.
func TestRunOutputKeepsOnlyTheAnswer(t *testing.T) {
	out := strings.Join([]string{
		`{"type":"step_start","timestamp":1788340599915,"sessionID":"ses_1"}`,
		`{"type":"text","part":{"id":"prt_1","text":"DONE "},"sessionID":"ses_1"}`,
		`{"type":"text","part":{"id":"prt_1","text":"PONG"},"sessionID":"ses_1"}`,
		`{"type":"step_finish","tokens":{"total":62165,"input":61569}}`,
		`not json at all`,
	}, "\n")
	if got := textFromRunJSON([]byte(out)); got != "DONE PONG" {
		t.Errorf("text = %q, want %q", got, "DONE PONG")
	}
}

// ==================== Transcript ====================

func TestParseLineCoversEveryRoleThePluginWrites(t *testing.T) {
	cases := []struct {
		line string
		want transcript.MessageRole
		body string
	}{
		{`{"ts":"t","role":"user","content":"hello"}`, transcript.RoleUser, "hello"},
		{`{"ts":"t","role":"assistant","content":"hi"}`, transcript.RoleAssistant, "hi"},
		{`{"ts":"t","role":"tool_use","tool":"bash","summary":"ls -la"}`, transcript.RoleToolUse, "ls -la"},
		{`{"ts":"t","role":"tool_result","tool":"bash","summary":"total 0"}`, transcript.RoleToolResult, "total 0"},
	}
	for i, c := range cases {
		msgs := ParseLine([]byte(c.line), i)
		if len(msgs) != 1 {
			t.Fatalf("%s parsed to %d messages, want 1", c.line, len(msgs))
		}
		if msgs[0].Role != c.want || msgs[0].Content != c.body || msgs[0].Seq != i {
			t.Errorf("parsed %+v, want role %s content %q seq %d", msgs[0], c.want, c.body, i)
		}
	}
}

// A line the plugin did not write must not become an empty message: a
// transcript of blanks is worse than a short one.
func TestParseLineRejectsWhatItDoesNotUnderstand(t *testing.T) {
	for _, line := range []string{
		`{"ts":"t","role":"reasoning","content":"thinking"}`,
		`{"ts":"t","role":"user"}`,
		`not json`,
		``,
	} {
		if msgs := ParseLine([]byte(line), 0); len(msgs) != 0 {
			t.Errorf("%q produced %+v, want nothing", line, msgs)
		}
	}
}

// ==================== Install ====================

// The config is the user's, and on a real machine it is JSONC carrying the
// reasoning behind every setting. Round-tripping it through encoding/json
// would load correctly and delete all of it.
func TestInstallPreservesCommentsAndOtherPlugins(t *testing.T) {
	home := configHome(t)
	cfgPath := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	const before = `{
  "$schema": "https://opencode.ai/config.json",

  // PLUGINS (paths relative to this config file)
  "plugin": [
    "opencode-routines",
    // superset-notify: desktop notifications when sessions need attention.
    "./plugins/superset-notify.js"
  ],
  "model": "google-vertex-anthropic/claude-opus-5@default"
}
`
	writeSeed(t, cfgPath, before)

	if err := New(7654).InstallHooks(provider.ScopeUser); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	after := readFile(t, cfgPath)
	for _, keep := range []string{
		"// PLUGINS (paths relative to this config file)",
		"// superset-notify: desktop notifications when sessions need attention.",
		`"opencode-routines"`,
		`"./plugins/superset-notify.js"`,
		`"$schema": "https://opencode.ai/config.json"`,
		`"model": "google-vertex-anthropic/claude-opus-5@default"`,
	} {
		if !strings.Contains(after, keep) {
			t.Errorf("install destroyed %s\n--- config ---\n%s", keep, after)
		}
	}
	if !strings.Contains(after, pluginEntry()) {
		t.Errorf("install did not add helios\n--- config ---\n%s", after)
	}
	if _, err := os.Stat(pluginPath()); err != nil {
		t.Errorf("install wrote no plugin at %s: %v", pluginPath(), err)
	}
}

// Run twice by an upgrade, a `helios setup`, and a user who typed the command
// again — three entries in the array would load the plugin three times.
func TestInstallIsIdempotent(t *testing.T) {
	home := configHome(t)
	cfgPath := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	writeSeed(t, cfgPath, "{\n  \"plugin\": [\"opencode-routines\"]\n}\n")
	p := New(7654)
	for i := range 3 {
		if err := p.InstallHooks(provider.ScopeUser); err != nil {
			t.Fatalf("InstallHooks #%d: %v", i, err)
		}
	}
	if n := strings.Count(readFile(t, cfgPath), pluginEntry()); n != 1 {
		t.Errorf("plugin listed %d times, want 1\n%s", n, readFile(t, cfgPath))
	}
}

// No config at all is the fresh-install case, and the file helios writes has
// to be one opencode can parse.
func TestInstallCreatesAConfigWhenThereIsNone(t *testing.T) {
	home := configHome(t)
	if err := New(7654).InstallHooks(provider.ScopeUser); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	cfgPath := filepath.Join(home, ".config", "opencode", "opencode.json")
	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(readFile(t, cfgPath)), &cfg); err != nil {
		t.Fatalf("helios wrote a config opencode cannot parse: %v", err)
	}
	plugins, _ := cfg["plugin"].([]interface{})
	if len(plugins) != 1 || plugins[0] != pluginEntry() {
		t.Errorf("plugin array = %+v", plugins)
	}
}

// Uninstalling helios is not permission to unload the user's plugins.
func TestRemovePreservesOtherPlugins(t *testing.T) {
	home := configHome(t)
	cfgPath := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	writeSeed(t, cfgPath, `{
  "plugin": [
    "opencode-routines",
    // a comment that belongs to the entry below it
    "./plugins/superset-notify.js"
  ]
}
`)
	p := New(7654)
	if err := p.InstallHooks(provider.ScopeUser); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if err := p.RemoveHooks(); err != nil {
		t.Fatalf("RemoveHooks: %v", err)
	}

	after := readFile(t, cfgPath)
	if strings.Contains(after, pluginEntry()) {
		t.Errorf("uninstall left helios behind\n%s", after)
	}
	for _, keep := range []string{
		`"opencode-routines"`,
		`"./plugins/superset-notify.js"`,
		"// a comment that belongs to the entry below it",
	} {
		if !strings.Contains(after, keep) {
			t.Errorf("uninstall destroyed %s\n%s", keep, after)
		}
	}
	if _, err := os.Stat(pluginPath()); !os.IsNotExist(err) {
		t.Error("uninstall left the plugin file on disk")
	}
	// Still parseable after the splice, which is the thing a text edit can
	// plausibly get wrong.
	if err := json.Unmarshal([]byte(stripComments(after)), &map[string]interface{}{}); err != nil {
		t.Errorf("config no longer parses after removal: %v\n%s", err, after)
	}
}

// Project scope needs no config edit at all: opencode auto-discovers every
// .js in .opencode/plugin. Worth a test because it is the one branch where
// helios touches nothing the user curates.
func TestProjectScopeInstallsWithoutTouchingConfig(t *testing.T) {
	home := configHome(t)
	cfgPath := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	const before = "{\n  \"plugin\": [\"opencode-routines\"]\n}\n"
	writeSeed(t, cfgPath, before)

	dir := t.TempDir()
	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })

	if err := New(7654).InstallHooks(provider.ScopeProject); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".opencode", "plugin", "helios.js")); err != nil {
		t.Errorf("no project plugin: %v", err)
	}
	if got := readFile(t, cfgPath); got != before {
		t.Errorf("a project install edited the user's config:\n%s", got)
	}
}

// ==================== Health ====================

// A plugin on disk that no config loads is not installed, however complete the
// file looks. That combination is what a hand-deleted config line leaves
// behind, and reporting it healthy hides the one thing that is wrong.
func TestHealthNeedsBothTheFileAndTheConfigEntry(t *testing.T) {
	home := configHome(t)
	p := New(7654)
	if err := p.InstallHooks(provider.ScopeUser); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if !p.HookHealth().Installed {
		t.Fatal("a complete install did not report installed")
	}

	cfgPath := filepath.Join(home, ".config", "opencode", "opencode.json")
	writeSeed(t, cfgPath, "{}\n")
	h := p.HookHealth()
	if h.Installed {
		t.Error("a plugin no config loads was reported as installed")
	}
	if !strings.Contains(h.Detail, "does not load it") {
		t.Errorf("detail = %q, which does not say what is wrong", h.Detail)
	}
}

// The version is the hash of the rendered plugin, so a plugin from an older
// helios reports itself without anyone remembering to bump a constant.
func TestHealthDetectsAStalePlugin(t *testing.T) {
	configHome(t)
	p := New(7654)
	if err := p.InstallHooks(provider.ScopeUser); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if !p.HookHealth().Current {
		t.Fatal("a fresh install is not current")
	}
	if err := os.WriteFile(pluginPath(), []byte("// last month's plugin\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if h := p.HookHealth(); h.Current {
		t.Error("an old plugin reported itself as current")
	}
}

// A placeholder that reaches the installed plugin is a silent failure: the
// file still parses, opencode still loads it, and every POST goes to the
// literal host "__HELIOS_ENDPOINT__". Nothing reports it — the daemon simply
// never hears from the agent. So the invariant is checked here rather than
// trusted to whoever adds the fifth placeholder.
func TestNoPlaceholderSurvivesRendering(t *testing.T) {
	// The header comment says "__HELIOS_*__" when talking about them, which is
	// prose rather than a placeholder; a real one is upper snake case.
	pattern := regexp.MustCompile(`__HELIOS_[A-Z0-9_]+__`)

	if len(pattern.FindAllString(pluginSource, -1)) == 0 {
		t.Fatal("the asset has no placeholders at all; this test would pass vacuously")
	}
	if left := pattern.FindAllString(renderPlugin(7654), -1); len(left) > 0 {
		t.Errorf("unsubstituted in the installed plugin: %q", left)
	}
}

// A plugin rendered for one port must not vouch for a daemon on another: the
// endpoint is baked into the file, so the old one posts into the void.
func TestPluginIsRenderedForItsOwnPort(t *testing.T) {
	configHome(t)
	if err := New(7654).InstallHooks(provider.ScopeUser); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if !strings.Contains(readFile(t, pluginPath()), "http://localhost:7654/hooks/opencode") {
		t.Error("the rendered plugin does not point at the daemon's port")
	}
	if New(9999).HookHealth().Current {
		t.Error("a plugin rendered for another port reported itself as current")
	}
	if New(7654).HookConfigHash() == New(9999).HookConfigHash() {
		t.Error("two ports rendered to the same hash")
	}
}

// The daemon writes the evidence and the setup TUI, in its own process, reads
// it. Held only in memory it would report "not effective" for ever to every
// process that was not the daemon.
func TestHookEvidenceIsVisibleToAnotherProcess(t *testing.T) {
	home := configHome(t)
	SetStateDir(home)
	resetHookEvidence()
	t.Cleanup(resetHookEvidence)

	p := New(7654)
	if err := p.InstallHooks(provider.ScopeUser); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if p.HookHealth().Effective {
		t.Fatal("an install nothing has called reported itself effective")
	}
	noteHookReceived()

	// A fresh process: nothing in memory, only what is on disk.
	hookEvidence.mu.Lock()
	hookEvidence.last = time.Time{}
	hookEvidence.mu.Unlock()

	if !New(7654).HookHealth().Effective {
		t.Error("evidence did not survive into a process that never saw the hook")
	}
}

// ==================== Hooks ====================

// The id opencode minted is the only thing `opencode --session` accepts, and
// the plugin is the only place helios can learn it.
func TestSessionStartRecordsTheIDOpencodeMinted(t *testing.T) {
	env := newHookEnv(t)
	env.post(t, handleSessionStart, "session/start", map[string]interface{}{
		"session_id": "ses_abc",
		"cwd":        "/work",
		"model":      "github-copilot/claude-opus-5",
	})

	sess, err := env.db.GetSession("helios-1")
	if err != nil || sess == nil {
		t.Fatalf("session not registered: %v", err)
	}
	if sess.Source != "opencode" {
		t.Errorf("source = %q", sess.Source)
	}
	got, err := env.db.SessionByResumeID("ses_abc")
	if err != nil || got == nil || got.SessionID != "helios-1" {
		t.Errorf("the opencode id was not recorded against the helios row: %+v %v", got, err)
	}
}

// The header is what binds a hook to a row helios minted before opencode had
// an id at all. Without it the daemon would file a second, ghost session.
func TestHooksWithoutTheHeaderFallBackToTheOpencodeID(t *testing.T) {
	env := newHookEnv(t)
	body, _ := json.Marshal(map[string]interface{}{"session_id": "ses_solo", "cwd": "/work"})
	req := httptest.NewRequest("POST", "/hooks/opencode/session/start", nil)
	w := httptest.NewRecorder()
	handleSessionStart(env.ctx, w, req, body)

	if w.Code != 200 {
		t.Fatalf("hook answered %d, want 200", w.Code)
	}
	if sess, _ := env.db.GetSession("ses_solo"); sess == nil {
		t.Error("a session started outside helios was not tracked under its own id")
	}
}

// The prompt is what a session is named and listed by, and PromptSubmitted is
// the only proof one landed — a wake that never sees it retypes the prompt.
func TestPromptSubmitRecordsTheUsersWords(t *testing.T) {
	env := newHookEnv(t)
	var submitted []string
	env.ctx.PromptSubmitted = func(id string) { submitted = append(submitted, id) }

	env.post(t, handleSessionStart, "session/start", map[string]interface{}{
		"session_id": "ses_abc", "cwd": "/work",
	})
	env.post(t, handlePromptSubmit, "prompt/submit", map[string]interface{}{
		"session_id": "ses_abc", "cwd": "/work", "prompt": "fix the auth bug",
	})

	sess, _ := env.db.GetSession("helios-1")
	if sess == nil || sess.Status != "active" {
		t.Fatalf("session = %+v, want active", sess)
	}
	if sess.Label(80) != "fix the auth bug" {
		t.Errorf("label = %q, want the prompt", sess.Label(80))
	}
	if !slices.Equal(submitted, []string{"helios-1"}) {
		t.Errorf("PromptSubmitted fired %v, want once for helios-1", submitted)
	}
}

// session/end runs on a context whose Terminal is nil whenever the daemon
// builds one for a single non-blocking call, so the nil path is the real one
// rather than a defensive branch.
func TestSessionEndTerminatesWithoutATerminal(t *testing.T) {
	env := newHookEnv(t)
	env.post(t, handleSessionStart, "session/start", map[string]interface{}{
		"session_id": "ses_abc", "cwd": "/work",
	})
	env.post(t, handleSessionEnd, "session/end", map[string]interface{}{
		"session_id": "ses_abc", "cwd": "/work",
	})

	sess, _ := env.db.GetSession("helios-1")
	if sess == nil || sess.Status != "terminated" {
		t.Errorf("session = %+v, want terminated", sess)
	}
}

func TestStopRaisesACardAndSettlesTheSession(t *testing.T) {
	env := newHookEnv(t)
	env.post(t, handleSessionStart, "session/start", map[string]interface{}{
		"session_id": "ses_abc", "cwd": "/work",
	})
	env.post(t, handleStop, "stop", map[string]interface{}{
		"session_id": "ses_abc", "cwd": "/work", "detail": "all done",
	})

	sess, _ := env.db.GetSession("helios-1")
	if sess == nil || sess.Status != "idle" {
		t.Errorf("session status = %+v, want idle", sess)
	}
	notifs, err := env.db.ListNotifications("opencode", "", "opencode.done")
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if len(notifs) != 1 {
		t.Fatalf("stop raised %d completion notices, want 1", len(notifs))
	}
	if notifs[0].Detail == nil || *notifs[0].Detail != "all done" {
		t.Errorf("detail = %+v, want the agent's own last word", notifs[0].Detail)
	}
}

// The blocking path, and the reason this provider exists in the shape it does:
// the plugin holds opencode's permission open until this returns.
func TestPermissionBlocksUntilSomebodyAnswers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		action     string
		wantDecide string
		wantScope  string
	}{
		{"approve once", "approve", "allow", ""},
		{"approve always", "approve_always", "allow", "always"},
		{"deny", "deny", "deny", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newHookEnv(t)
			env.post(t, handleSessionStart, "session/start", map[string]interface{}{
				"session_id": "ses_abc", "cwd": "/work",
			})

			answered := make(chan struct{})
			go func() {
				defer close(answered)
				env.answerPendingPermission(t, tc.action)
			}()

			body, _ := json.Marshal(map[string]interface{}{
				"session_id": "ses_abc",
				"cwd":        "/work",
				"request_id": "per_1",
				"tool_name":  "bash",
				"call_id":    "call_1",
				"patterns":   []string{"rm *"},
			})
			req := httptest.NewRequest("POST", "/hooks/opencode/permission", nil)
			req.Header.Set(heliosSessionHeader, "helios-1")
			w := httptest.NewRecorder()
			handlePermission(env.ctx, w, req, body)
			<-answered

			if w.Code != 200 {
				t.Fatalf("hook answered %d, want 200", w.Code)
			}
			var got permResponse
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("reply is not JSON the plugin can read: %v (%s)", err, w.Body)
			}
			if got.Decision != tc.wantDecide || got.Scope != tc.wantScope {
				t.Errorf("reply = %+v, want decision %q scope %q", got, tc.wantDecide, tc.wantScope)
			}
		})
	}
}

// "always" writes a permission rule into the user's config. Anything short of
// an explicit ask for it must fall to the narrow answer.
func TestApprovalScopeFailsNarrow(t *testing.T) {
	for _, response := range []string{"", "{}", `{"scope":"once"}`, "not json", `{"scope":"ALWAYS"}`} {
		d := &notifications.Decision{Status: "approved"}
		if response != "" {
			d.Response = json.RawMessage(response)
		}
		if got := decisionScope(d); got != "" {
			t.Errorf("response %q widened the approval to %q", response, got)
		}
	}
	d := &notifications.Decision{Status: "approved", Response: json.RawMessage(`{"scope":"always"}`)}
	if got := decisionScope(d); got != scopeAlways {
		t.Errorf("an explicit always was narrowed to %q", got)
	}
}

// A tool that ran was permitted by somebody — most often the person sitting at
// the terminal, who can always answer opencode's own prompt. The card on the
// phone has to come down with it.
func TestToolPostRetractsThePermissionForThatCall(t *testing.T) {
	env := newHookEnv(t)
	env.post(t, handleSessionStart, "session/start", map[string]interface{}{
		"session_id": "ses_abc", "cwd": "/work",
	})

	mine := env.pendingPermission(t, "call_1", "bash")
	other := env.pendingPermission(t, "call_2", "bash")

	env.post(t, handleToolPost, "tool/post", map[string]interface{}{
		"session_id": "ses_abc", "cwd": "/work", "tool_name": "bash", "call_id": "call_1",
	})

	if env.status(t, mine) == "pending" {
		t.Error("the card for the tool that ran is still pending")
	}
	if env.status(t, other) != "pending" {
		t.Error("a concurrent call's card was retracted by the wrong tool_post")
	}
}

// ==================== Capabilities ====================

// The registry finds capabilities by assertion, so an omitted interface is how
// a provider says "not supported". These two are omitted on purpose and the
// reasons are in opencode.go; asserting it keeps a later edit from adding a
// stub that reports a capability opencode does not have.
func TestUnsupportedCapabilitiesAreAbsentRatherThanStubbed(t *testing.T) {
	var p interface{} = New(0)
	if _, ok := p.(provider.ScreenWatcher); ok {
		t.Error("opencode asks nothing at start-up; a screen matcher can only produce false positives")
	}
	if _, ok := p.(provider.Discoverer); ok {
		t.Error("discovery would need opencode's private SQLite schema")
	}
}

func TestCapabilitiesOpencodeDoesSupportArePresent(t *testing.T) {
	var p interface{} = New(0)
	for name, ok := range map[string]bool{
		"Hooker":        assertOK[provider.Hooker](p),
		"HookInstaller": assertOK[provider.HookInstaller](p),
		"Actor":         assertOK[provider.Actor](p),
		"Resumer":       assertOK[provider.Resumer](p),
		"Moder":         assertOK[provider.Moder](p),
		"ModelLister":   assertOK[provider.ModelLister](p),
		"Transcriber":   assertOK[provider.Transcriber](p),
		"Titler":        assertOK[provider.Titler](p),
		"SmallModel":    assertOK[provider.SmallModel](p),
		"Queuer":        assertOK[provider.Queuer](p),
		"Narrator":      assertOK[provider.Narrator](p),
		"Commander":     assertOK[provider.Commander](p),
	} {
		if !ok {
			t.Errorf("%s is not implemented", name)
		}
	}
}

func assertOK[T any](v interface{}) bool {
	_, ok := v.(T)
	return ok
}

// Every route the provider serves must have a handler, and every blocking one
// must be answerable — a route with no action route behind it raises a card
// nobody can clear.
func TestPermissionRouteHasAnActionBehindIt(t *testing.T) {
	p := New(0)
	if _, ok := p.HookRoutes()["permission"]; !ok {
		t.Fatal("no permission route")
	}
	route, ok := p.ActionRoutes()["opencode.permission"]
	if !ok {
		t.Fatal("the permission card has no action route; nothing could answer it")
	}
	if !route.Blocking {
		t.Error("the permission card is not marked blocking")
	}
}

// ==================== Helpers ====================

func hasFlagPair(argv []string, flag, value string) bool {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) && argv[i+1] == value {
			return true
		}
	}
	return false
}

// configHome points every path this package computes at a temporary directory,
// so a test never reads or writes the developer's own opencode config.
func configHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func writeSeed(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// stripComments is only good enough to hand a JSONC document to encoding/json
// in a test. It is not what the installer uses; see jsonc.go.
func stripComments(src string) string {
	var out strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

type hookEnv struct {
	db  *store.Store
	mgr *notifications.Manager
	ctx *provider.HookContext
}

func newHookEnv(t *testing.T) *hookEnv {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mgr := notifications.NewManager(db)
	return &hookEnv{
		db:  db,
		mgr: mgr,
		ctx: &provider.HookContext{
			DB:              db,
			Mgr:             mgr,
			Notify:          func(string, interface{}) {},
			Report:          func(provider.ReportEvent) {},
			SessionStarted:  func(string) {},
			PromptSubmitted: func(string) {},
		},
	}
}

func (e *hookEnv) post(t *testing.T, h provider.HookHandler, route string, body map[string]interface{}) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal %s: %v", route, err)
	}
	req := httptest.NewRequest("POST", "/hooks/opencode/"+route, nil)
	req.Header.Set(heliosSessionHeader, "helios-1")
	w := httptest.NewRecorder()
	h(e.ctx, w, req, raw)
	if w.Code != 200 {
		t.Fatalf("%s answered %d, want 200: %s", route, w.Code, w.Body)
	}
}

// pendingPermission files a permission card directly, for the tests that care
// about what retracts one rather than about how it was raised.
func (e *hookEnv) pendingPermission(t *testing.T, callID, tool string) string {
	t.Helper()
	id := notifications.GenerateNotificationID()
	payload, _ := json.Marshal(map[string]string{"call_id": callID, "tool_name": tool})
	body := string(payload)
	notif := &store.Notification{
		ID:            id,
		Source:        "opencode",
		SourceSession: "helios-1",
		CWD:           "/work",
		Type:          "opencode.permission",
		Status:        "pending",
		Title:         &tool,
		Payload:       &body,
	}
	if err := e.mgr.CreateNotification(notif); err != nil {
		t.Fatalf("create notification: %v", err)
	}
	return id
}

func (e *hookEnv) status(t *testing.T, id string) string {
	t.Helper()
	notif, err := e.db.GetNotification(id)
	if err != nil || notif == nil {
		t.Fatalf("get notification %s: %v", id, err)
	}
	return notif.Status
}

// answerPendingPermission waits for the handler to file its card and then
// answers it the way a phone would — through the action route, so the test
// covers the path a client actually takes.
func (e *hookEnv) answerPendingPermission(t *testing.T, action string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := e.mgr.ListNotifications("opencode", "pending", "opencode.permission")
		if err == nil && len(pending) > 0 {
			body, _ := json.Marshal(map[string]string{"action": action})
			decision, err := handlePermissionAction(&pending[0], body)
			if err != nil {
				t.Errorf("action %q: %v", action, err)
				return
			}
			if err := e.mgr.Resolve(pending[0].ID, decision, "phone"); err != nil {
				t.Errorf("resolve: %v", err)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("no permission card was ever filed")
}
