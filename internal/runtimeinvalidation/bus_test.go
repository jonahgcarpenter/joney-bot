package runtimeinvalidation

import (
	"sync"
	"testing"
)

func TestBusSubscribePublishAndUnsubscribe(t *testing.T) {
	bus := NewBus()
	var mu sync.Mutex
	count := 0
	unsubscribe := bus.Subscribe(func(event Event) {
		if !event.CloseConnections {
			t.Fatal("subscriber received wrong event")
		}
		mu.Lock()
		count++
		mu.Unlock()
	})
	bus.Publish(Event{CloseConnections: true})
	unsubscribe()
	unsubscribe()
	bus.Publish(Event{CloseConnections: true})
	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("subscriber call count=%d want 1", count)
	}
}

func TestBusPublishesAllSubscribers(t *testing.T) {
	bus := NewBus()
	called := 0
	bus.Subscribe(func(Event) { called++ })
	bus.Subscribe(func(Event) { called++ })
	bus.Publish(Event{})
	if called != 2 {
		t.Fatalf("subscriber calls=%d want 2", called)
	}
}

func TestBusSubscriberCanChangeSubscriptions(t *testing.T) {
	bus := NewBus()
	first, second := 0, 0
	var unsubscribe func()
	unsubscribe = bus.Subscribe(func(Event) {
		first++
		unsubscribe()
		bus.Subscribe(func(Event) { second++ })
	})
	bus.Publish(Event{})
	if first != 1 || second != 0 {
		t.Fatalf("publication did not use subscriber snapshot: first=%d second=%d", first, second)
	}
	bus.Publish(Event{})
	if first != 1 || second != 1 {
		t.Fatalf("subscription changes were not applied: first=%d second=%d", first, second)
	}
}
