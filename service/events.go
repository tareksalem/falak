package service

import "time"

// Manager event types (one per ManagerEvent.Type emitted by the manager).
const (
	EventServiceCreated                = "service.created"
	EventServiceUpdated                = "service.updated"
	EventServiceDeleted                = "service.deleted"
	EventServiceReceived               = "service.received"
	EventServiceBackendResolved        = "service.backend_resolved"
	EventServiceBackendUnresolved      = "service.backend_unresolved"
	EventServiceBackendIdentityChanged = "service.backend_identity_changed"
	EventServiceCanaryStep             = "service.canary_step"
	EventServiceCanaryAborted          = "service.canary_aborted"
	EventServiceBlueGreenFlip          = "service.blue_green_flip"
)

// Meta keys carried on strategy-progress ManagerEvents.
const (
	// MetaCanaryTarget identifies the target backend of the canary step.
	MetaCanaryTarget = "canary_target"
	// MetaCanaryAbortReason carries the human-readable abort condition.
	MetaCanaryAbortReason = "canary_abort_reason"
	// MetaBlueGreenFrom identifies the previous active backend of a flip.
	MetaBlueGreenFrom = "blue_green_from"
	// MetaBlueGreenTo identifies the new active backend of a flip.
	MetaBlueGreenTo = "blue_green_to"
)

// Meta keys carried on ManagerEvents for events that need context not
// captured on the Service snapshot itself.
const (
	MetaBackendName       = "backend"
	MetaPreviousCapsuleID = "previous_capsule_id"
	MetaNewCapsuleID      = "new_capsule_id"
)

// ManagerEvent is emitted by the manager on lifecycle and resolution
// actions. Meta carries side-band context that doesn't fit on the
// Service snapshot itself (e.g. which backend changed identity).
type ManagerEvent struct {
	Type      string
	ServiceID ServiceID
	Service   *Service
	Timestamp time.Time
	Meta      map[string]string
}

// EventHandler receives every ManagerEvent published by the manager.
type EventHandler func(ManagerEvent)
