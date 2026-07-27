package hub

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPublish_DeliversToSubscriber(t *testing.T) {
	h := New(10)
	ch, replay, unsubscribe := h.Subscribe(nil)
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

	_, replay, unsubscribe := h.Subscribe(nil)
	defer unsubscribe()

	require.Len(t, replay, 2)
	require.Equal(t, "two", replay[0].Message)
	require.Equal(t, "three", replay[1].Message)
}

func TestPublish_SlowSubscriberDoesNotBlock(t *testing.T) {
	h := New(10)
	ch, _, unsubscribe := h.Subscribe(nil)
	defer unsubscribe()

	// Fill the subscriber's channel buffer (capacity == bufferSize, 10 here
	// — see Subscribe) well past its limit without reading, then publish
	// some more — Publish must not block on the full channel.
	for i := 0; i < 100; i++ {
		h.Publish(Event{Message: "spam"})
	}

	require.NotEmpty(t, ch)
}

func TestWriter_PublishesOneEventPerCompleteLine(t *testing.T) {
	h := New(10)
	ch, _, unsubscribe := h.Subscribe(nil)
	defer unsubscribe()

	w := h.Writer("app_log")
	n, err := w.Write([]byte("first line\nsecond line\n"))
	require.NoError(t, err)
	require.Equal(t, len("first line\nsecond line\n"), n)

	ev1 := <-ch
	require.Equal(t, "app_log", ev1.Source)
	require.Equal(t, "first line", ev1.Message)

	ev2 := <-ch
	require.Equal(t, "second line", ev2.Message)
}

func TestWriter_BuffersPartialLineAcrossWrites(t *testing.T) {
	h := New(10)
	ch, _, unsubscribe := h.Subscribe(nil)
	defer unsubscribe()

	w := h.Writer("llm_log")
	_, err := w.Write([]byte("partial "))
	require.NoError(t, err)
	require.Empty(t, ch, "no newline yet: nothing published")

	_, err = w.Write([]byte("line\n"))
	require.NoError(t, err)

	ev := <-ch
	require.Equal(t, "partial line", ev.Message)
}

func TestSubscribe_FilterExcludesNonMatchingEventsFromChannelAndReplay(t *testing.T) {
	h := New(10)
	h.Publish(Event{Source: "chat", UserID: "alice", Message: "old-alice"})
	h.Publish(Event{Source: "chat", UserID: "bob", Message: "old-bob"})

	onlyAlice := func(ev Event) bool { return ev.Source == "chat" && ev.UserID == "alice" }
	ch, replay, unsubscribe := h.Subscribe(onlyAlice)
	defer unsubscribe()

	require.Len(t, replay, 1)
	require.Equal(t, "old-alice", replay[0].Message)

	h.Publish(Event{Source: "chat", UserID: "bob", Message: "new-bob"})
	h.Publish(Event{Source: "error", Message: "unrelated"})
	h.Publish(Event{Source: "chat", UserID: "alice", Message: "new-alice"})

	ev := <-ch
	require.Equal(t, "new-alice", ev.Message, "bob's and error events must never reach alice's filtered channel")
	require.Empty(t, ch, "only the one matching event should have been delivered")
}

func TestSubscribe_FilterDoesNotStarveUnderUnrelatedBurst(t *testing.T) {
	h := New(2) // small enough that an unfiltered subscriber would drop fast
	onlyAlice := func(ev Event) bool { return ev.Source == "chat" && ev.UserID == "alice" }
	ch, _, unsubscribe := h.Subscribe(onlyAlice)
	defer unsubscribe()

	for i := 0; i < 50; i++ {
		h.Publish(Event{Source: "chat", UserID: "bob", Message: "spam"})
	}
	h.Publish(Event{Source: "chat", UserID: "alice", Message: "hi"})

	ev := <-ch
	require.Equal(t, "hi", ev.Message, "a burst of another user's events must never fill alice's channel and drop her own event")
}

func TestWriter_ConcurrentWritesDoNotCorruptLines(t *testing.T) {
	h := New(1000)
	ch, _, unsubscribe := h.Subscribe(nil)
	defer unsubscribe()

	w := h.Writer("app_log")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = w.Write([]byte("line\n"))
		}()
	}
	wg.Wait()

	count := 0
	for {
		select {
		case ev := <-ch:
			require.Equal(t, "line", ev.Message)
			count++
		default:
			require.Equal(t, 50, count)
			return
		}
	}
}
