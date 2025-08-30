package watcher

import (
	"sync"
	"time"
)

// WatcherMetrics tracks metrics for the resource watcher
type WatcherMetrics struct {
	mu sync.RWMutex

	// Event counts
	EventsProcessed     int64
	EventsFiltered      int64
	NotificationsSent   int64
	NotificationsFailed int64

	// Deployment-specific metrics
	DeploymentChangesDetected int64
	DeploymentChangesIgnored  int64

	// Performance metrics
	StartupSyncTime time.Duration
	LastEventTime   time.Time

	// Field change metrics
	FieldChanges map[string]int64
}

// NewWatcherMetrics creates a new metrics instance
func NewWatcherMetrics() *WatcherMetrics {
	return &WatcherMetrics{
		FieldChanges: make(map[string]int64),
	}
}

// RecordEventProcessed records a processed event
func (m *WatcherMetrics) RecordEventProcessed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.EventsProcessed++
	m.LastEventTime = time.Now()
}

// RecordEventFiltered records a filtered event
func (m *WatcherMetrics) RecordEventFiltered() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.EventsFiltered++
}

// RecordNotificationSent records a successful notification
func (m *WatcherMetrics) RecordNotificationSent() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.NotificationsSent++
}

// RecordNotificationFailed records a failed notification
func (m *WatcherMetrics) RecordNotificationFailed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.NotificationsFailed++
}

// RecordDeploymentChange records a deployment field change
func (m *WatcherMetrics) RecordDeploymentChange(fieldName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DeploymentChangesDetected++
	m.FieldChanges[fieldName]++
}

// RecordDeploymentChangeIgnored records an ignored deployment change
func (m *WatcherMetrics) RecordDeploymentChangeIgnored() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DeploymentChangesIgnored++
}

// GetMetrics returns a copy of the current metrics
func (m *WatcherMetrics) GetMetrics() *WatcherMetrics {
	m.mu.RLock()
	defer m.mu.RUnlock()

	metrics := &WatcherMetrics{
		EventsProcessed:           m.EventsProcessed,
		EventsFiltered:            m.EventsFiltered,
		NotificationsSent:         m.NotificationsSent,
		NotificationsFailed:       m.NotificationsFailed,
		DeploymentChangesDetected: m.DeploymentChangesDetected,
		DeploymentChangesIgnored:  m.DeploymentChangesIgnored,
		StartupSyncTime:           m.StartupSyncTime,
		LastEventTime:             m.LastEventTime,
		FieldChanges:              make(map[string]int64),
	}

	// Copy the field changes map
	for k, v := range m.FieldChanges {
		metrics.FieldChanges[k] = v
	}

	return metrics
}
