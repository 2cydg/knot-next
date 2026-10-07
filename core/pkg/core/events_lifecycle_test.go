package core

import "testing"

func TestGlobalSubscriberLimitAndClose(t *testing.T) {
	b := NewEventBus()
	var cancels []func()
	var events []<-chan Event
	for i := 0; i < maxEventSubscribers; i++ {
		ch, cancel, err := b.Subscribe()
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ch)
		cancels = append(cancels, cancel)
	}
	if _, _, err := b.Subscribe(); err == nil {
		t.Fatal("unbounded subscribers")
	}
	for i := 0; i < 1000; i++ {
		b.Publish(Event{Type: "test"})
	}
	cancels[0]()
	cancels[0]()
	_, cancel, err := b.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	cancels = append(cancels, cancel)
	b.Close()
	b.Close()
	for _, cancel := range cancels {
		cancel()
		cancel()
	}
	for _, ch := range events {
		for range ch {
		}
	}
	if len(b.subscribers) != 0 {
		t.Fatal("subscriber leak")
	}
	if _, _, err := b.Subscribe(); err == nil {
		t.Fatal("closed bus accepts subscription")
	}
}
