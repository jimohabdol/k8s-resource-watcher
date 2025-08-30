package notifier

// NotificationEvent represents a resource event to be notified
type NotificationEvent struct {
	EventType    string
	ResourceKind string
	ResourceName string
	Namespace    string
}

// Notifier defines the interface for sending notifications
type Notifier interface {
	SendNotification(event NotificationEvent) error
}
