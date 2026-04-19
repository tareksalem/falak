package core

import (
	"context"
	"time"
)

const (
	// watchBufferSize is the bounded buffer per watch client. If the
	// client is slow and the buffer fills, oldest events are dropped
	// and a synthetic error event is sent so the client knows to re-list.
	watchBufferSize = 512
)

// Watch opens a watch stream for the specified resource type. Returns a
// channel of events. The channel is closed when ctx is cancelled.
// Events are buffered up to watchBufferSize; if the client is slow,
// oldest events are dropped.
func (c *Core) Watch(ctx context.Context, resourceType string) (<-chan WatchEvent, error) {
	sourceCh, err := c.node.WatchEvents(ctx, resourceType)
	if err != nil {
		return nil, err
	}

	// Bounded buffer between the event source and the client.
	out := make(chan WatchEvent, watchBufferSize)

	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-sourceCh:
				if !ok {
					return
				}
				select {
				case out <- ev:
				default:
					// Buffer full — drop oldest by reading one, then write.
					select {
					case <-out:
					default:
					}
					// Send an error event so the client knows it missed events.
					select {
					case out <- WatchEvent{
						Type:      WatchEventTypeEnum.Error(),
						Timestamp: time.Now(),
					}:
					default:
					}
					// Try to send the original event.
					select {
					case out <- ev:
					default:
					}
				}
			}
		}
	}()

	return out, nil
}
