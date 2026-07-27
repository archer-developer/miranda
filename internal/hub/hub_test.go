package hub

import (
	"sync"
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
	ch, _, unsubscribe := h.Subscribe()
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
	ch, _, unsubscribe := h.Subscribe()
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

func TestWriter_ConcurrentWritesDoNotCorruptLines(t *testing.T) {
	h := New(1000)
	ch, _, unsubscribe := h.Subscribe()
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
