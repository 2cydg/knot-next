package core

import (
	"errors"
	"sync"
	"time"
)

const defaultEventBuffer = 64
const maxEventSubscribers = 64

var ErrEventSubscription = errors.New("event subscription unavailable: service closed or subscriber limit reached")

type Event struct {
	Type       string         `json:"type"`
	Resource   string         `json:"resource"`
	ResourceID string         `json:"resource_id,omitempty"`
	Level      string         `json:"level,omitempty"`
	Time       time.Time      `json:"time"`
	Data       map[string]any `json:"data,omitempty"`
}

type EventBus struct {
	mu          sync.Mutex
	subscribers map[chan Event]struct{}
	closed      bool
}

func NewEventBus() *EventBus {
	return &EventBus{subscribers: map[chan Event]struct{}{}}
}

func (b *EventBus) Subscribe() (<-chan Event, func(), error) {
	ch := make(chan Event, defaultEventBuffer)
	b.mu.Lock()
	if b.closed || len(b.subscribers) >= maxEventSubscribers {
		close(ch)
		b.mu.Unlock()
		return ch, func() {}, ErrEventSubscription
	}
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			if _, ok := b.subscribers[ch]; ok {
				delete(b.subscribers, ch)
				close(ch)
			}
			b.mu.Unlock()
		})
	}
	return ch, cancel, nil
}

func (b *EventBus) Publish(event Event) {
	if event.Type == "" {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	for ch := range b.subscribers {
		select {
		case ch <- cloneEvent(event):
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- cloneEvent(event):
			default:
			}
		}
	}
}

func (b *EventBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for ch := range b.subscribers {
		close(ch)
		delete(b.subscribers, ch)
	}
}

func cloneEvent(event Event) Event {
	out := event
	if event.Data != nil {
		out.Data = make(map[string]any, len(event.Data))
		for key, value := range event.Data {
			out.Data[key] = value
		}
	}
	return out
}
