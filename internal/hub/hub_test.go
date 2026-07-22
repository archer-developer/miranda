package hub

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPublish_DeliversToSubscriber(t *testing.T) {
	h := New(10)
	ch, replay, unsubscribe := h.Subscribe()
	defer unsubscribe()
	require.Empty(t, replay)

	h.Publish(Event{Source: "test", Message: "hello"})

	ev := <-ch
	require.Equal(t, "hello", ev.Message)
}

func TestSubscribe_ReplaysBufferedEvents(t *testing.T) {
	h := New(2)
	h.Publish(Event{Message: "one"})
	h.Publish(Event{Message: "two"})
	h.Publish(Event{Message: "three"}) // buffer size 2: "one" is evicted

	_, replay, unsubscribe := h.Subscribe()
	defer unsubscribe()

	require.Len(t, replay, 2)
	require.Equal(t, "two", replay[0].Message)
	require.Equal(t, "three", replay[1].Message)
}

func TestPublish_SlowSubscriberDoesNotBlock(t *testing.T) {
	h := New(10)
	ch, _, unsubscribe := h.Subscribe()
	defer unsubscribe()

	// Fill the subscriber's channel buffer (capacity 64) without reading,
	// then publish once more — Publish must not block on the full channel.
	for i := 0; i < 100; i++ {
		h.Publish(Event{Message: "spam"})
	}

	require.NotEmpty(t, ch)
}
