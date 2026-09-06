// Package invalidation distributes transport-neutral runtime invalidations.
package invalidation

import "sync"

// Event identifies runtime state that must be discarded after a security mutation.
type Event struct {
	ExternalIdentities []string
	SessionIDs         []string
	CloseConnections   bool
}

// Bus synchronously publishes runtime invalidations to subscribers.
type Bus struct {
	mu          sync.RWMutex
	nextID      uint64
	subscribers map[uint64]func(Event)
}

// NewBus creates an empty invalidation bus.
func NewBus() *Bus {
	return &Bus{subscribers: make(map[uint64]func(Event))}
}

// Subscribe registers a handler and returns an idempotent unsubscribe function.
func (b *Bus) Subscribe(handler func(Event)) func() {
	if b == nil || handler == nil {
		return func() {}
	}
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.subscribers[id] = handler
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subscribers, id)
			b.mu.Unlock()
		})
	}
}

// Publish delivers an event to the subscribers present at publication time.
func (b *Bus) Publish(event Event) {
	if b == nil {
		return
	}
	b.mu.RLock()
	handlers := make([]func(Event), 0, len(b.subscribers))
	for _, handler := range b.subscribers {
		handlers = append(handlers, handler)
	}
	b.mu.RUnlock()
	for _, handler := range handlers {
		handler(event)
	}
}
