package hub

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func receive(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case event := <-ch:
		return event
	case <-time.After(time.Second):
		t.Fatal("no event received")
		return Event{}
	}
}

func TestPublishReachesEverySubscriber(t *testing.T) {
	h := New()
	a, cancelA := h.Subscribe()
	b, cancelB := h.Subscribe()
	defer cancelA()
	defer cancelB()

	h.Publish(Event{Type: TypeNginxLogIndexReady, Data: NginxLogIndexReadyData{LogPath: "/x"}})

	assert.Equal(t, TypeNginxLogIndexReady, receive(t, a).Type)
	assert.Equal(t, TypeNginxLogIndexReady, receive(t, b).Type)
}

func TestSlowSubscriberDropsOldestNeverBlocks(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := range subscriberBuffer * 3 {
			h.Publish(Event{Type: "n", Data: i})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher blocked on a slow subscriber")
	}

	assert.Len(t, ch, subscriberBuffer)
	first := receive(t, ch)
	assert.Equal(t, subscriberBuffer*2, first.Data, "the oldest events are the ones dropped")
}

func TestCancelClosesTheChannelOnce(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe()
	cancel()
	cancel()

	_, open := <-ch
	assert.False(t, open)
	assert.Zero(t, h.Subscribers())
}

func TestStatusPublishesChangesOnce(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe()
	defer cancel()

	var changes []bool
	status := NewStatus(h, func(indexing bool) { changes = append(changes, indexing) })

	status.SetIndexing(true)
	status.SetIndexing(true)
	status.SetIndexing(false)

	first := receive(t, ch)
	assert.Equal(t, TypeProcessingStatus, first.Type)
	assert.Equal(t, ProcessingStatusData{NginxLogIndexing: true}, first.Data)
	second := receive(t, ch)
	assert.Equal(t, ProcessingStatusData{NginxLogIndexing: false}, second.Data)
	assert.Empty(t, ch)
	require.Equal(t, []bool{true, false}, changes)
	assert.False(t, status.Indexing())
}
