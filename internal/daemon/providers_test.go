package daemon

import (
	"testing"

	"github.com/kamrul1157024/helios/internal/backend"
	"github.com/kamrul1157024/helios/internal/provider"
	"github.com/kamrul1157024/helios/internal/terminal"
)

// backendHungryProvider is the minimal provider that drives a terminal: it
// accepts a backend, like every real provider that answers a phone approval or
// holds a queued prompt.
type backendHungryProvider struct {
	id  string
	got backend.Backend
}

func (p *backendHungryProvider) Info() provider.Info {
	return provider.Info{ID: p.id, Name: p.id}
}

func (p *backendHungryProvider) Launch(provider.SessionSpec) (provider.Launch, error) {
	return provider.Launch{}, nil
}

func (p *backendHungryProvider) SetBackend(b backend.Backend) { p.got = b }

// wireBackends must reach every registered provider that accepts a backend,
// resolving them by the setter interface they implement rather than by name.
// A provider that drives a session terminal but is handed no backend returns
// "no terminal" from QueuePrompt and its terminal actions; a unit test that
// injects the backend directly cannot see that, so this test exercises the
// daemon's own wiring against a provider discovered only through the registry.
func TestWireBackendsWiresEveryCapableProvider(t *testing.T) {
	fake := &backendHungryProvider{id: "fake-backend-provider"}
	if err := provider.Register(fake); err != nil {
		t.Fatalf("register fake provider: %v", err)
	}
	t.Cleanup(func() { provider.Deregister(fake.id) })

	registry := terminal.NewRegistry(t.TempDir(), func(string, string, []string, map[string]string) error { return nil })
	term := backend.NewHost(registry)

	wireBackends(term)

	if fake.got == nil {
		t.Fatal("wireBackends left a provider that accepts a backend without one")
	}
}
