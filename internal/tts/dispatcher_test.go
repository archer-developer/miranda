package tts

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/hub"
)

// fakeProvider is a scriptable Provider: each Speak call returns the next
// entry in errs (repeating the last one once exhausted), and records the
// text it was called with.
type fakeProvider struct {
	mu    sync.Mutex
	errs  []error
	calls []string
}

func (p *fakeProvider) Speak(ctx context.Context, text, entityID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, text)
	if len(p.errs) == 0 {
		return nil
	}
	err := p.errs[0]
	if len(p.errs) > 1 {
		p.errs = p.errs[1:]
	}
	return err
}

func (p *fakeProvider) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.calls))
	copy(out, p.calls)
	return out
}

func TestDispatcher_SpeakOne_UsesPrimaryOnSuccess(t *testing.T) {
	primary := &fakeProvider{}
	fallback := &fakeProvider{}
	d := &Dispatcher{primary: primary, fallback: fallback, hub: hub.New(10, nil)}

	err := d.speakOne(context.Background(), "привет", "media_player.kitchen")
	require.NoError(t, err)
	require.Equal(t, []string{"привет"}, primary.Calls())
	require.Empty(t, fallback.Calls())
}

func TestDispatcher_SpeakOne_FallsBackOnQuotaExceeded(t *testing.T) {
	primary := &fakeProvider{errs: []error{fmt.Errorf("wrap: %w", ErrQuotaExceeded)}}
	fallback := &fakeProvider{}
	d := &Dispatcher{primary: primary, fallback: fallback, hub: hub.New(10, nil)}

	err := d.speakOne(context.Background(), "привет", "media_player.kitchen")
	require.NoError(t, err)
	require.Equal(t, []string{"привет"}, primary.Calls())
	require.Equal(t, []string{"привет"}, fallback.Calls())
}

func TestDispatcher_SpeakOne_NonQuotaErrorNeverFallsBack(t *testing.T) {
	primary := &fakeProvider{errs: []error{errors.New("network unreachable")}}
	fallback := &fakeProvider{}
	d := &Dispatcher{primary: primary, fallback: fallback, hub: hub.New(10, nil)}

	err := d.speakOne(context.Background(), "привет", "media_player.kitchen")
	require.Error(t, err)
	require.Equal(t, []string{"привет"}, primary.Calls())
	require.Empty(t, fallback.Calls(), "a non-quota error must never try the fallback provider")
}

func TestDispatcher_SpeakOne_QuotaExceededWithNoFallbackConfiguredReturnsError(t *testing.T) {
	primary := &fakeProvider{errs: []error{ErrQuotaExceeded}}
	d := &Dispatcher{primary: primary, fallback: nil, hub: hub.New(10, nil)}

	err := d.speakOne(context.Background(), "привет", "media_player.kitchen")
	require.ErrorIs(t, err, ErrQuotaExceeded)
}

// TestDispatcher_SpeakEnqueuesAsynchronously proves Speak returns
// immediately without waiting for the primary provider to actually finish —
// the whole point of routing it through Player instead of calling primary
// directly.
func TestDispatcher_SpeakEnqueuesAsynchronously(t *testing.T) {
	release := make(chan struct{})
	primary := &blockingProvider{release: release}
	d := NewDispatcher(primary, nil, &stopTrackingHA{}, "kitchen", hub.New(10, nil), nil)

	done := make(chan struct{})
	go func() {
		d.Speak(context.Background(), "привет")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Speak blocked instead of enqueuing and returning immediately")
	}

	close(release)
	require.Eventually(t, func() bool { return primary.called() }, time.Second, time.Millisecond)
}

// blockingProvider's Speak blocks until release is closed, to prove a
// caller of Dispatcher.Speak never waits on it.
type blockingProvider struct {
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (p *blockingProvider) Speak(ctx context.Context, text, entityID string) error {
	<-p.release
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return nil
}

func (p *blockingProvider) called() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls > 0
}

// TestDispatcher_StopDoesNotPanic is a smoke test confirming Stop wires
// through to Player.Stop without panicking; the queue-clearing and
// cancellation contract is exercised in player_test.go.
func TestDispatcher_StopDoesNotPanic(t *testing.T) {
	d := NewDispatcher(&fakeProvider{}, nil, &stopTrackingHA{}, "kitchen", hub.New(10, nil), nil)
	d.Stop()
}
