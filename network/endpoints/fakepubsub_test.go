package endpoints

import (
	"bytes"
	"context"
	"errors"
	"sync"
)

// fakePubSub is an in-memory PubSub for tests. Each topic owns a list
// of subscriber channels; Publish fans the bytes out to every channel
// synchronously (so tests can read the result without timing dances).
type fakePubSub struct {
	mu          sync.Mutex
	subscribers map[string][]chan []byte
	published   map[string][][]byte
	failNext    map[string]error // optional: inject Publish errors per topic
	closed      bool
}

func newFakePubSub() *fakePubSub {
	return &fakePubSub{
		subscribers: map[string][]chan []byte{},
		published:   map[string][][]byte{},
		failNext:    map[string]error{},
	}
}

func (f *fakePubSub) Publish(ctx context.Context, topic string, data []byte) error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return errors.New("fakePubSub closed")
	}
	if err, ok := f.failNext[topic]; ok && err != nil {
		delete(f.failNext, topic)
		f.mu.Unlock()
		return err
	}
	clone := append([]byte(nil), data...)
	f.published[topic] = append(f.published[topic], clone)
	subs := append([]chan []byte(nil), f.subscribers[topic]...)
	f.mu.Unlock()
	for _, ch := range subs {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ch <- append([]byte(nil), data...):
		}
	}
	return nil
}

func (f *fakePubSub) Subscribe(ctx context.Context, topic string) (<-chan []byte, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, errors.New("fakePubSub closed")
	}
	ch := make(chan []byte, 64)
	f.subscribers[topic] = append(f.subscribers[topic], ch)
	f.mu.Unlock()

	// Close the subscriber channel when the context is canceled so the
	// readLoop in Subscriber exits naturally.
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		defer f.mu.Unlock()
		subs := f.subscribers[topic]
		for i, c := range subs {
			if c == ch {
				f.subscribers[topic] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
		close(ch)
	}()
	return ch, nil
}

// publishedFor returns a deep-copy of every payload Publish recorded
// for topic so test assertions don't race with subsequent publishes.
func (f *fakePubSub) publishedFor(topic string) [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	src := f.published[topic]
	out := make([][]byte, len(src))
	for i, b := range src {
		out[i] = append([]byte(nil), b...)
	}
	return out
}

func (f *fakePubSub) injectPublishError(topic string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[topic] = err
}

// stubSigner / stubVerifier sign by HMAC-like fixed prefix; sufficient
// for envelope round-trip and tamper-detection tests.
type stubSigner struct {
	prefix []byte
}

func (s *stubSigner) Sign(content []byte) ([]byte, error) {
	out := make([]byte, 0, len(s.prefix)+len(content))
	out = append(out, s.prefix...)
	out = append(out, content...)
	return out, nil
}

type stubVerifier struct {
	prefix     []byte
	allowedIDs map[string]bool
}

func newStubVerifier(prefix string, allowed ...string) *stubVerifier {
	v := &stubVerifier{prefix: []byte(prefix), allowedIDs: map[string]bool{}}
	for _, id := range allowed {
		v.allowedIDs[id] = true
	}
	return v
}

func (v *stubVerifier) Verify(senderID string, content, signature []byte) bool {
	if !v.allowedIDs[senderID] {
		return false
	}
	if len(signature) < len(v.prefix) {
		return false
	}
	if !bytes.Equal(signature[:len(v.prefix)], v.prefix) {
		return false
	}
	return bytes.Equal(signature[len(v.prefix):], content)
}
