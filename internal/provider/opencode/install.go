package opencode

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kamrul1157024/helios/internal/provider"
)

// pluginSource is the plugin helios installs, carried in the binary so
// `helios hooks install` needs no checkout — the same reason the skill is
// embedded.
//
//go:embed plugin/helios.js
var pluginSource string

// opencode has no hook table.
//
// Claude and Codex both read a JSON file of shell commands, which is why their
// installers write config and nothing else. opencode's only extension point is
// a JavaScript module loaded into the agent's own process, so installing hooks
// here means installing a program: render it, write it, and tell opencode
// where it is.
//
// That is a better position than it sounds. A shell hook gets one request and
// one reply; this runs inside the agent and can answer a permission out of
// band, which is what makes "allow always" possible at all.

// HookTimeoutSeconds is the budget the plugin gets for a blocking permission.
// Derived from the one the daemon itself waits: helios has to give up first,
// or the plugin walks away from a request that is still on someone's phone.
var HookTimeoutSeconds = int(PluginDecisionTimeout / time.Second)

// pluginVersion identifies the rendered plugin, and is what the health check
// compares. Content-addressed rather than hand-bumped: a version constant
// someone forgets to raise reports healthy while running last month's plugin.
func pluginVersion(port int) string {
	return hashOf(renderPlugin(port))[:12]
}

// renderPlugin substitutes the installed copy's three parameters.
//
// A rendered constant rather than a config file read at load: the plugin runs
// before helios knows it exists, so anything it needs has to be in it already.
func renderPlugin(port int) string {
	dir, _ := TranscriptDir()
	out := strings.NewReplacer(
		"__HELIOS_ENDPOINT__", fmt.Sprintf("http://localhost:%d/hooks/opencode", port),
		"__HELIOS_TRANSCRIPT_DIR__", dir,
		"__HELIOS_DECISION_TIMEOUT_MS__", fmt.Sprintf("%d", HookTimeoutSeconds*1000),
	).Replace(pluginSource)
	// Substituted last: the version is the hash of everything above it, so it
	// cannot be part of what is hashed.
	return strings.ReplaceAll(out, "__HELIOS_VERSION__", hashOf(out)[:12])
}

// ==================== Paths ====================

// configDir is opencode's global configuration directory, honouring
// XDG_CONFIG_HOME as opencode itself does.
func configDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "opencode")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "opencode")
}

// configPath is the global config helios edits.
//
// opencode loads opencode.json and opencode.jsonc from that directory and
// merges both, so the file to edit is whichever the user already has. When
// neither exists a plain .json is created: helios has no comments to preserve
// in a file it wrote itself, and .json is the name the schema documents.
func configPath() string {
	dir := configDir()
	for _, name := range []string{"opencode.jsonc", "opencode.json"} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return filepath.Join(dir, "opencode.json")
}

// pluginPath is where the rendered plugin lives.
//
// Under helios's own directory, not opencode's. The file is helios's — it is
// rendered from a build, it is overwritten on upgrade, and uninstalling helios
// should take it away. Keeping it out of the user's config directory means
// RemoveHooks never deletes anything from a directory the user curates.
func pluginPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".helios", "opencode", "helios.js")
}

// pluginEntry is the string written into the config's plugin array.
//
// A file:// URL rather than a relative path. opencode resolves a relative
// entry against the config that declared it, and the plugin does not live
// beside that config; an absolute URL is the same string wherever the config
// moves to.
func pluginEntry() string {
	path := pluginPath()
	if path == "" {
		return ""
	}
	return "file://" + filepath.ToSlash(path)
}

// projectPluginPath is the project-scoped install.
//
// No config edit at all: opencode auto-discovers every .js in a project's
// .opencode/plugin directory. This is the one scope where the plugin needs no
// entry anywhere, which is worth the branch.
func projectPluginPath() string {
	return filepath.Join(".opencode", "plugin", "helios.js")
}

// ==================== Install ====================

// InstallHooks writes the plugin and, for a user-scope install, names it in
// opencode's config.
func (p *Provider) InstallHooks(scope provider.Scope) error {
	rendered := renderPlugin(p.port)

	if scope == provider.ScopeProject {
		path := projectPluginPath()
		if err := writeFile(path, rendered, 0o644); err != nil {
			return err
		}
		return nil
	}

	path := pluginPath()
	if path == "" {
		return fmt.Errorf("no home directory")
	}
	if err := writeFile(path, rendered, 0o644); err != nil {
		return err
	}

	cfgPath := configPath()
	src := "{}"
	if data, err := os.ReadFile(cfgPath); err == nil {
		src = string(data)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", cfgPath, err)
	}

	out, changed, err := addPluginEntry(src, pluginEntry())
	if err != nil {
		return fmt.Errorf("edit %s: %w", cfgPath, err)
	}
	if !changed {
		return nil
	}
	if err := writeFile(cfgPath, out, 0o644); err != nil {
		return err
	}

	// Read back and re-check. opencode drops a config it cannot parse with a
	// warning the user will never see if the TUI is what printed it, and the
	// symptom would be a daemon that simply never hears from the agent.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("verify %s: %w", cfgPath, err)
	}
	if !hasPluginEntry(string(data), pluginEntry()) {
		return fmt.Errorf("verify %s: plugin entry was not written as intended", cfgPath)
	}
	return nil
}

func writeFile(path, content string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create dir for %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// RemoveHooks deletes helios's plugin and its config entry, and nothing else.
func (p *Provider) RemoveHooks() error {
	if path := pluginPath(); path != "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove plugin: %w", err)
		}
	}
	if err := os.Remove(projectPluginPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove project plugin: %w", err)
	}

	cfgPath := configPath()
	data, err := os.ReadFile(cfgPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", cfgPath, err)
	}
	out, changed, err := removePluginEntry(string(data), pluginEntry())
	if err != nil {
		return fmt.Errorf("edit %s: %w", cfgPath, err)
	}
	if !changed {
		return nil
	}
	return writeFile(cfgPath, out, 0o644)
}

// ==================== Health ====================

// HookHealth reports whether opencode will actually run our plugin.
//
// Effective is a separate question here for a reason of its own. Codex's is
// trust: it reads a hook table and declines to run it. opencode's is loading:
// a plugin whose import throws is logged once, inside the agent's own process,
// and every session after that runs with no plugin at all while the config
// still names it. Both fail the same way from outside — the file is right, the
// config is right, and nothing ever arrives — so both are answered the same
// way, from evidence that a hook was received.
func (p *Provider) HookHealth() provider.HookHealth {
	h := provider.HookHealth{}

	path := pluginPath()
	if path == "" {
		h.Detail = "no home directory"
		return h
	}
	installed, err := os.ReadFile(path)
	if err != nil {
		h.Detail = "no " + path + "; run `helios hooks install`"
		return h
	}

	cfg, err := os.ReadFile(configPath())
	if err != nil || !hasPluginEntry(string(cfg), pluginEntry()) {
		h.Detail = "plugin is written but " + configPath() +
			" does not load it; run `helios hooks install`"
		return h
	}
	h.Installed = true

	h.Current = string(installed) == renderPlugin(p.port)
	if !h.Current {
		h.Detail = "plugin is from an older helios; run `helios hooks install`"
		return h
	}

	h.Effective = hooksSeenRecently()
	if !h.Effective {
		h.Detail = "set up; opencode loads the plugin when it next starts"
	}
	return h
}

// HookConfigHash is the hash of what this build would install.
func (p *Provider) HookConfigHash() string { return pluginVersion(p.port) }

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ==================== Evidence ====================

// evidenceFile is where the last-hook timestamp is persisted.
//
// A file rather than the daemon's database, for the reason Codex's is: two
// processes need it. The daemon writes it when a hook arrives and the setup
// TUI, running on its own, reads it to decide what to report.
const evidenceFile = "opencode-hooks-seen"

// stateDir is where helios keeps its own files. Set by whoever registers the
// provider, since this package cannot import the daemon that owns the path.
var stateDir atomic.Pointer[string]

// SetStateDir tells the provider where to record that the plugin is running.
func SetStateDir(dir string) { stateDir.Store(&dir) }

func evidencePath() string {
	dir := stateDir.Load()
	if dir == nil || *dir == "" {
		return ""
	}
	return filepath.Join(*dir, evidenceFile)
}

// hookEvidence records that the plugin reached the daemon.
//
// Guarded: every inbound hook writes it, from its own goroutine, while the
// health endpoint reads it.
var hookEvidence struct {
	mu   sync.Mutex
	last time.Time
}

// noteHookReceived is called from every hook route this provider serves.
func noteHookReceived() {
	now := time.Now()
	hookEvidence.mu.Lock()
	hookEvidence.last = now
	hookEvidence.mu.Unlock()

	path := evidencePath()
	if path == "" {
		return
	}
	stamp := []byte(now.UTC().Format(time.RFC3339))
	if err := os.WriteFile(path, stamp, 0o644); err != nil {
		log.Printf("opencode: record hook evidence: %v", err)
	}
}

// hookEvidenceTTL is how long a received hook vouches for the install. Long
// enough to span an idle session, short enough that a plugin the user removed
// stops being reported as working.
const hookEvidenceTTL = 24 * time.Hour

func hooksSeenRecently() bool {
	hookEvidence.mu.Lock()
	last := hookEvidence.last
	hookEvidence.mu.Unlock()

	if last.IsZero() {
		if path := evidencePath(); path != "" {
			if raw, err := os.ReadFile(path); err == nil {
				last, _ = time.Parse(time.RFC3339, strings.TrimSpace(string(raw)))
			}
		}
	}
	return !last.IsZero() && time.Since(last) < hookEvidenceTTL
}

// resetHookEvidence forgets that any hook has arrived. For tests.
func resetHookEvidence() {
	hookEvidence.mu.Lock()
	defer hookEvidence.mu.Unlock()
	hookEvidence.last = time.Time{}
	if path := evidencePath(); path != "" {
		os.Remove(path)
	}
}
