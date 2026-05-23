package node

import (
	"github.com/tareksalem/falak/service"
	"github.com/tareksalem/falak/service/strategy"
)

// strategyEmitter satisfies strategy.EventEmitter by forwarding each
// progress callback onto the service.Manager's event handler chain.
// External subscribers (API/SSE, metrics, observability) see Canary /
// BlueGreen progress through the same path the manager already
// publishes Created/Updated/Deleted on.
type strategyEmitter struct {
	manager *service.Manager
}

// EmitCanaryStep forwards the post-step weight snapshot. The weights
// argument is intentionally dropped at this layer — subscribers that
// need it read the strategy engine directly via StrategyFor.
func (e *strategyEmitter) EmitCanaryStep(id service.ServiceID, target string, _ strategy.LiveWeights) {
	if e.manager == nil {
		return
	}
	e.manager.EmitStrategyEvent(service.EventServiceCanaryStep, id, map[string]string{
		service.MetaCanaryTarget: target,
	})
}

// EmitCanaryAborted forwards an abort_on hit.
func (e *strategyEmitter) EmitCanaryAborted(id service.ServiceID, reason string) {
	if e.manager == nil {
		return
	}
	e.manager.EmitStrategyEvent(service.EventServiceCanaryAborted, id, map[string]string{
		service.MetaCanaryAbortReason: reason,
	})
}

// EmitBlueGreenFlip forwards the flip from→to so dashboards can show
// active/draining state without polling.
func (e *strategyEmitter) EmitBlueGreenFlip(id service.ServiceID, from, to string) {
	if e.manager == nil {
		return
	}
	e.manager.EmitStrategyEvent(service.EventServiceBlueGreenFlip, id, map[string]string{
		service.MetaBlueGreenFrom: from,
		service.MetaBlueGreenTo:   to,
	})
}
