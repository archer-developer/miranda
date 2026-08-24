package hub

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPublish_DeliversToSubscriber(t *testing.T) {
	h := New(10, nil)
	ch, replay, unsubscribe := h.Subscribe(nil)
	defer unsubscribe()
	require.Empty(t, replay)

	h.Publish(Event{Source: "test", Message: "hello"})

	ev := <-ch
	require.Equal(t, "hello", ev.Message)
}

func TestSubscribe_ReplaysBufferedEvents(t *testing.T) {
	h := New(2, nil)
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
	h := New(10, nil)
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
	h := New(10, nil)
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
	h := New(10, nil)
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
	h := New(10, nil)
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
	h := New(2, nil) // small enough that an unfiltered subscriber would drop fast
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

func TestSubscribe_ReplayTrimmedByByteCap(t *testing.T) {
	// bufferSize (10) is large enough that only the byte cap should do any
	// trimming here — each message is 5 bytes, so a 12-byte cap keeps at
	// most the last 2.
	h := New(10, map[string]SourceLimit{"app_log": {MaxBytes: 12}})
	h.Publish(Event{Source: "app_log", Message: "one12"})
	h.Publish(Event{Source: "app_log", Message: "two34"})
	h.Publish(Event{Source: "app_log", Message: "three"})

	_, replay, unsubscribe := h.Subscribe(nil)
	defer unsubscribe()

	require.Len(t, replay, 2)
	require.Equal(t, "two34", replay[0].Message)
	require.Equal(t, "three", replay[1].Message)
}

func TestSubscribe_ByteCapNeverEvictsTheOnlyEvent(t *testing.T) {
	// A single event larger than MaxBytes must still be kept — the cap
	// trims history, it doesn't block publishing.
	h := New(10, map[string]SourceLimit{"app_log": {MaxBytes: 1}})
	h.Publish(Event{Source: "app_log", Message: "this message is way over the cap"})

	_, replay, unsubscribe := h.Subscribe(nil)
	defer unsubscribe()

	require.Len(t, replay, 1)
}

func TestSubscribe_PerSourceLimitsTrimIndependently(t *testing.T) {
	// "llm_log"-style: capped by block count, any size. "app_log"-style:
	// capped by total size, any count. Published interleaved to verify
	// each source's trim only ever looks at its own buffer.
	h := New(100, map[string]SourceLimit{
		"llm_log": {MaxCount: 2},
		"app_log": {MaxBytes: 6}, // "aaa"+"bbb" fits, a third 3-byte line evicts one
	})

	h.Publish(Event{Source: "llm_log", Message: "call-1"})
	h.Publish(Event{Source: "app_log", Message: "aaa"})
	h.Publish(Event{Source: "llm_log", Message: "call-2"})
	h.Publish(Event{Source: "app_log", Message: "bbb"})
	h.Publish(Event{Source: "llm_log", Message: "call-3"}) // evicts call-1
	h.Publish(Event{Source: "app_log", Message: "ccc"})    // evicts aaa

	_, replay, unsubscribe := h.Subscribe(nil)
	defer unsubscribe()

	var llmMessages, appMessages []string
	for _, ev := range replay {
		switch ev.Source {
		case "llm_log":
			llmMessages = append(llmMessages, ev.Message)
		case "app_log":
			appMessages = append(appMessages, ev.Message)
		}
	}

	require.Equal(t, []string{"call-2", "call-3"}, llmMessages, "llm_log's count cap must evict its own oldest, unaffected by app_log's traffic")
	require.Equal(t, []string{"bbb", "ccc"}, appMessages, "app_log's byte cap must evict its own oldest, unaffected by llm_log's traffic")
}

func TestSubscribe_SourceWithNoExplicitLimitFallsBackToBufferSize(t *testing.T) {
	h := New(2, map[string]SourceLimit{"llm_log": {MaxCount: 100}})
	h.Publish(Event{Source: "assistant", Message: "one"})
	h.Publish(Event{Source: "assistant", Message: "two"})
	h.Publish(Event{Source: "assistant", Message: "three"}) // bufferSize 2: "one" evicted

	_, replay, unsubscribe := h.Subscribe(nil)
	defer unsubscribe()

	require.Len(t, replay, 2)
	require.Equal(t, "two", replay[0].Message)
	require.Equal(t, "three", replay[1].Message)
}

func TestWriter_ConcurrentWritesDoNotCorruptLines(t *testing.T) {
	h := New(1000, nil)
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
