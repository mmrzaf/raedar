package store

// Severity says how serious an incident or event is.
type Severity string

// The severities, from least to most serious.
const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// EventKind says what an event records.
type EventKind string

// The kinds of events.
const (
	// EventDeploy is a new image running in a slot.
	EventDeploy EventKind = "deploy"
	// EventRecreated is a container recreated from the same image.
	EventRecreated EventKind = "recreated"
	// EventCrashed is a container that exited on its own with an error.
	EventCrashed EventKind = "crashed"
	// EventStopped is a container stopped on purpose.
	EventStopped EventKind = "stopped"
	// EventStarted is a container that started.
	EventStarted EventKind = "started"
	// EventHealth is a change of a container's health status.
	EventHealth EventKind = "health"
	// EventRemoved is a container that was destroyed.
	EventRemoved EventKind = "removed"
	// EventUnitFailed is a systemd unit that failed.
	EventUnitFailed EventKind = "unit_failed"
	// EventUnitRecovered is a failed systemd unit that recovered.
	EventUnitRecovered EventKind = "unit_recovered"
	// EventGap is a stretch of measurements that was lost.
	EventGap EventKind = "gap"
)

// DeliveryKind says what a delivery carries.
type DeliveryKind string

// The kinds of deliveries.
const (
	DeliveryOpened    DeliveryKind = "opened"
	DeliveryEscalated DeliveryKind = "escalated"
	DeliveryResolved  DeliveryKind = "resolved"
	DeliveryDigest    DeliveryKind = "digest"
	DeliveryTest      DeliveryKind = "test"
)
