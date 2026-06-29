package podman

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/runtime"
)

// eventStreamChanBuffer bounds the in-flight backlog between the Podman
// event decoder and the handler's consumer. A small buffer absorbs short
// consumer stalls (e.g. a MarkFailed round-trip) without blocking the
// decode loop; if the consumer falls permanently behind the decode loop
// blocks (with select on ctx) rather than growing memory unboundedly.
const eventStreamChanBuffer = 32

// podmanEvent is the wire shape of one record on the Podman /libpod/events
// newline-delimited JSON stream (verified against Podman 4.9.3). Only the
// fields Falak needs are decoded; the rest are ignored by encoding/json.
type podmanEvent struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Status string `json:"status"`
	Actor  struct {
		ID         string `json:"ID"`
		Attributes struct {
			Name              string `json:"name"`
			ContainerExitCode string `json:"containerExitCode"`
		} `json:"Attributes"`
	} `json:"Actor"`
	Time     int64 `json:"time"`
	TimeNano int64 `json:"timeNano"`
}

// mapAction maps a Podman action string onto a runtime.ContainerEventAction.
// The canonical source is the event's Action field; status mirrors it. Any
// action not relevant to crash/removal detection maps to Other (dropped by
// the consumer).
func mapAction(action string) runtime.ContainerEventAction {
	switch action {
	case "died":
		return runtime.ContainerEventActionEnum.Died()
	case "remove":
		return runtime.ContainerEventActionEnum.Removed()
	case "start":
		return runtime.ContainerEventActionEnum.Started()
	case "stop":
		return runtime.ContainerEventActionEnum.Stopped()
	default:
		return runtime.ContainerEventActionEnum.Other()
	}
}

// eventTime resolves the most precise timestamp available on a record,
// preferring the nanosecond field. Falls back to time.Now when the
// backend omits both (never observed, but defensive).
func (e podmanEvent) eventTime() time.Time {
	switch {
	case e.TimeNano > 0:
		return time.Unix(0, e.TimeNano)
	case e.Time > 0:
		return time.Unix(e.Time, 0)
	default:
		return time.Now()
	}
}

// Events streams container lifecycle events from Podman's /libpod/events
// endpoint. It opens a streaming GET filtered to container-type events,
// decodes the newline-delimited JSON, maps each record to a
// runtime.ContainerEvent (ContainerID from Actor.Attributes.name, exit
// code parsed from the containerExitCode string, Action via mapAction),
// and emits on a buffered channel. The channel is closed when ctx is
// cancelled, the stream reaches EOF, or a decode error occurs. The HTTP
// response body is always closed on every exit path.
func (r *Runtime) Events(ctx context.Context) (<-chan runtime.ContainerEvent, error) {
	q := url.Values{
		"stream":  {"true"},
		"filters": {`{"type":["container"]}`},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL("/events", q), nil)
	if err != nil {
		return nil, fmt.Errorf("podman events: %w", err)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("podman events: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("podman events: status %d", resp.StatusCode)
	}

	ch := make(chan runtime.ContainerEvent, eventStreamChanBuffer)
	go func() {
		defer resp.Body.Close()
		defer close(ch)

		dec := json.NewDecoder(resp.Body)
		for {
			var raw podmanEvent
			if err := dec.Decode(&raw); err != nil {
				// EOF or any decode error ends the stream; the consumer
				// reconnects with backoff. Log at Debug — a dropped event
				// stream is expected on socket restart and the reconcile
				// sweep is the correctness backstop.
				r.logger.Debug("podman: event stream closed", zap.Error(err))
				return
			}
			if raw.Type != "container" {
				continue
			}

			action := raw.Action
			if action == "" {
				action = raw.Status
			}

			var exitCode int
			if raw.Actor.Attributes.ContainerExitCode != "" {
				if code, parseErr := strconv.Atoi(raw.Actor.Attributes.ContainerExitCode); parseErr == nil {
					exitCode = code
				}
			}

			evt := runtime.ContainerEvent{
				ContainerID: raw.Actor.Attributes.Name,
				Action:      mapAction(action),
				ExitCode:    exitCode,
				Time:        raw.eventTime(),
			}

			select {
			case ch <- evt:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}
