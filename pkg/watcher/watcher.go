package watcher

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"k8s-resource-watcher/pkg/config"
	"k8s-resource-watcher/pkg/notifier"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// ResourceWatcher handles watching Kubernetes resources
type ResourceWatcher struct {
	client       dynamic.Interface
	config       *config.Config
	eventHandler func(event *ResourceEvent)
	metrics      *WatcherMetrics
	notifier     notifier.Notifier
	ctx          context.Context
	cancel       context.CancelFunc
	mu           sync.RWMutex
	watchers     map[string]*ResourceWatchState
	semaphore    chan struct{}       // Limit concurrent watchers
	eventBuffer  chan *ResourceEvent // Buffer for event batching

	// Enhanced features
	frequencyTracker *EventFrequencyTracker
	adaptiveCooldown *AdaptiveCooldown
	eventCorrelator  *EventCorrelator
	priorityQueue    *PriorityQueue
}

// WatcherMetrics tracks watcher statistics
type WatcherMetrics struct {
	EventsReceived    int64
	EventsProcessed   int64
	EventsSkipped     int64
	EventsBatched     int64
	EventsDropped     int64
	WatchErrors       int64
	WatchReconnects   int64
	BufferUtilization float64 // Percentage of buffer usage
	LastUpdateTime    time.Time
}

type EventFrequencyTracker struct {
	mu              sync.RWMutex
	eventCounts     map[string]int       // resourceKey - event count
	lastEventTimes  map[string]time.Time // resourceKey - last event time
	frequencyWindow time.Duration        // Time window for frequency calculation
}

type AdaptiveCooldown struct {
	mu              sync.RWMutex
	baseCooldown    time.Duration
	currentCooldown time.Duration
	lastAdjustment  time.Time
	adjustmentCount int
}

type EventCorrelator struct {
	mu                sync.RWMutex
	relatedEvents     map[string][]*ResourceEvent // correlationKey - events
	correlationWindow time.Duration
}

type PriorityQueue struct {
	mu     sync.RWMutex
	high   []*ResourceEvent
	medium []*ResourceEvent
	low    []*ResourceEvent
}

type EventPriority int

const (
	PriorityHigh EventPriority = iota
	PriorityMedium
	PriorityLow
)

func newEventFrequencyTracker() *EventFrequencyTracker {
	return &EventFrequencyTracker{
		eventCounts:     make(map[string]int),
		lastEventTimes:  make(map[string]time.Time),
		frequencyWindow: 60 * time.Second, // 1 minute window
	}
}

func newAdaptiveCooldown(baseCooldown time.Duration) *AdaptiveCooldown {
	return &AdaptiveCooldown{
		baseCooldown:    baseCooldown,
		currentCooldown: baseCooldown,
		lastAdjustment:  time.Now(),
	}
}

func newEventCorrelator() *EventCorrelator {
	return &EventCorrelator{
		relatedEvents:     make(map[string][]*ResourceEvent),
		correlationWindow: 30 * time.Second,
	}
}

func newPriorityQueue() *PriorityQueue {
	return &PriorityQueue{
		high:   make([]*ResourceEvent, 0),
		medium: make([]*ResourceEvent, 0),
		low:    make([]*ResourceEvent, 0),
	}
}

func (f *EventFrequencyTracker) recordEvent(resourceKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	now := time.Now()
	f.eventCounts[resourceKey]++
	f.lastEventTimes[resourceKey] = now

	// Clean up old entries
	go f.cleanupOldEntries()
}

func (f *EventFrequencyTracker) getEventFrequency(resourceKey string) float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()

	count, exists := f.eventCounts[resourceKey]
	if !exists {
		return 0
	}

	lastTime, exists := f.lastEventTimes[resourceKey]
	if !exists {
		return 0
	}

	timeSinceLastEvent := time.Since(lastTime)
	if timeSinceLastEvent > f.frequencyWindow {
		return 0
	}

	return float64(count) / timeSinceLastEvent.Seconds() * 60 // events per minute
}

func (f *EventFrequencyTracker) cleanupOldEntries() {
	f.mu.Lock()
	defer f.mu.Unlock()

	cutoff := time.Now().Add(-f.frequencyWindow)
	for key, lastTime := range f.lastEventTimes {
		if lastTime.Before(cutoff) {
			delete(f.eventCounts, key)
			delete(f.lastEventTimes, key)
		}
	}
}

func (a *AdaptiveCooldown) getCooldown(eventFrequency float64) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Adjust cooldown based on event frequency
	if eventFrequency > 10 {
		a.currentCooldown = a.baseCooldown * 2
	} else if eventFrequency > 5 {
		a.currentCooldown = a.baseCooldown * 3 / 2
	} else if eventFrequency < 1 {
		a.currentCooldown = a.baseCooldown / 2
	} else {
		a.currentCooldown = a.baseCooldown
	}

	return a.currentCooldown
}

func (e *EventCorrelator) addEvent(event *ResourceEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()

	correlationKey := fmt.Sprintf("%s:%s:%s", event.ResourceKind, event.Namespace, event.User)
	e.relatedEvents[correlationKey] = append(e.relatedEvents[correlationKey], event)

	// Clean up old events
	go e.cleanupOldEvents()
}

func (e *EventCorrelator) getCorrelatedEvents(event *ResourceEvent) []*ResourceEvent {
	e.mu.RLock()
	defer e.mu.RUnlock()

	correlationKey := fmt.Sprintf("%s:%s:%s", event.ResourceKind, event.Namespace, event.User)
	events, exists := e.relatedEvents[correlationKey]
	if !exists {
		return []*ResourceEvent{event}
	}

	// Return events within correlation window
	var recentEvents []*ResourceEvent
	cutoff := time.Now().Add(-e.correlationWindow)
	for _, evt := range events {
		if evt.Timestamp.After(cutoff) {
			recentEvents = append(recentEvents, evt)
		}
	}

	if len(recentEvents) == 0 {
		return []*ResourceEvent{event}
	}

	return recentEvents
}

func (e *EventCorrelator) cleanupOldEvents() {
	e.mu.Lock()
	defer e.mu.Unlock()

	cutoff := time.Now().Add(-e.correlationWindow)
	for key, events := range e.relatedEvents {
		var recentEvents []*ResourceEvent
		for _, event := range events {
			if event.Timestamp.After(cutoff) {
				recentEvents = append(recentEvents, event)
			}
		}
		if len(recentEvents) == 0 {
			delete(e.relatedEvents, key)
		} else {
			e.relatedEvents[key] = recentEvents
		}
	}
}

func (p *PriorityQueue) addEvent(event *ResourceEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch event.Priority {
	case PriorityHigh:
		p.high = append(p.high, event)
	case PriorityMedium:
		p.medium = append(p.medium, event)
	case PriorityLow:
		p.low = append(p.low, event)
	}
}

func (p *PriorityQueue) getNextEvent() *ResourceEvent {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Return high priority events first
	if len(p.high) > 0 {
		event := p.high[0]
		p.high = p.high[1:]
		return event
	}

	// Then medium priority
	if len(p.medium) > 0 {
		event := p.medium[0]
		p.medium = p.medium[1:]
		return event
	}

	// Finally low priority
	if len(p.low) > 0 {
		event := p.low[0]
		p.low = p.low[1:]
		return event
	}

	return nil
}

func (p *PriorityQueue) size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.high) + len(p.medium) + len(p.low)
}

func determineEventPriority(eventType watch.EventType, resourceKind string) EventPriority {
	switch eventType {
	case "DELETED":
		return PriorityHigh
	case "ADDED":
		return PriorityMedium
	case "MODIFIED":
		// Some resource types are more critical than others
		switch resourceKind {
		case "Secret", "ConfigMap":
			return PriorityMedium
		default:
			return PriorityLow
		}
	default:
		return PriorityLow
	}
}

func generateCorrelationID(event *ResourceEvent) string {
	return fmt.Sprintf("%s:%s:%s:%s", event.ResourceKind, event.Namespace, event.ResourceName, event.User)
}

func (w *ResourceWatcher) cleanupOldNotificationEntries(lastNotificationTime map[string]time.Time, notificationCooldown time.Duration) {
	cutoff := time.Now().Add(-notificationCooldown * 2)
	cleanedCount := 0

	for key, lastTime := range lastNotificationTime {
		if lastTime.Before(cutoff) {
			delete(lastNotificationTime, key)
			cleanedCount++
		}
	}

	if cleanedCount > 0 {
		log.Printf("Cleaned up %d old notification tracking entries", cleanedCount)
	}
}

// shouldNotifyForChange determines if a change should trigger a notification based on resource type
func (w *ResourceWatcher) shouldNotifyForChange(resourceKind string, oldObj, newObj *unstructured.Unstructured) bool {
	switch resourceKind {
	case "ConfigMap":
		return w.hasConfigMapDataChanged(oldObj, newObj)
	case "Secret":
		return w.hasSecretDataChanged(oldObj, newObj)
	case "Ingress":
		return w.hasIngressSpecChanged(oldObj, newObj)
	case "Deployment":
		return w.hasDeploymentImportantFieldsChanged(oldObj, newObj)
	default:
		return true
	}
}

// hasConfigMapDataChanged checks if ConfigMap data has changed
func (w *ResourceWatcher) hasConfigMapDataChanged(oldObj, newObj *unstructured.Unstructured) bool {
	oldData, oldExists := oldObj.Object["data"].(map[string]interface{})
	newData, newExists := newObj.Object["data"].(map[string]interface{})

	if !oldExists && !newExists {
		return false
	}
	if !oldExists || !newExists {
		return true
	}

	return !reflect.DeepEqual(oldData, newData)
}

// hasSecretDataChanged checks if Secret data has changed
func (w *ResourceWatcher) hasSecretDataChanged(oldObj, newObj *unstructured.Unstructured) bool {
	oldData, oldExists := oldObj.Object["data"].(map[string]interface{})
	newData, newExists := newObj.Object["data"].(map[string]interface{})

	if !oldExists && !newExists {
		return false
	}
	if !oldExists || !newExists {
		return true
	}

	return !reflect.DeepEqual(oldData, newData)
}

// hasIngressSpecChanged checks if Ingress spec has changed
func (w *ResourceWatcher) hasIngressSpecChanged(oldObj, newObj *unstructured.Unstructured) bool {
	oldSpec, oldExists := oldObj.Object["spec"].(map[string]interface{})
	newSpec, newExists := newObj.Object["spec"].(map[string]interface{})

	if !oldExists && !newExists {
		return false
	}
	if !oldExists || !newExists {
		return true
	}

	return !reflect.DeepEqual(oldSpec, newSpec)
}

// hasDeploymentImportantFieldsChanged checks if important Deployment fields have changed
func (w *ResourceWatcher) hasDeploymentImportantFieldsChanged(oldObj, newObj *unstructured.Unstructured) bool {
	// Get the spec sections
	oldSpec, oldExists := oldObj.Object["spec"].(map[string]interface{})
	newSpec, newExists := newObj.Object["spec"].(map[string]interface{})

	if !oldExists || !newExists {
		return true // If spec doesn't exist, consider it a change
	}

	// Check if template spec has changed (this includes containers, volumes, etc.)
	oldTemplate, oldTemplateExists := oldSpec["template"].(map[string]interface{})
	newTemplate, newTemplateExists := newSpec["template"].(map[string]interface{})

	if !oldTemplateExists || !newTemplateExists {
		return true
	}

	oldTemplateSpec, oldTemplateSpecExists := oldTemplate["spec"].(map[string]interface{})
	newTemplateSpec, newTemplateSpecExists := newTemplate["spec"].(map[string]interface{})

	if !oldTemplateSpecExists || !newTemplateSpecExists {
		return true
	}

	importantFields := w.config.Watcher.GetDeploymentImportantFields()

	for _, field := range importantFields {
		if w.hasFieldChanged(oldTemplateSpec, newTemplateSpec, field) {
			log.Printf("Deployment important field '%s' changed", field)
			return true
		}
	}

	oldStrategy := oldSpec["strategy"]
	newStrategy := newSpec["strategy"]
	if !reflect.DeepEqual(oldStrategy, newStrategy) {
		log.Printf("Deployment strategy changed")
		return true
	}

	log.Printf("Deployment change detected but no important fields modified - likely status change (pod restart)")
	return false
}

// hasFieldChanged checks if a field has changed, handling both direct and nested field paths
func (w *ResourceWatcher) hasFieldChanged(oldSpec, newSpec map[string]interface{}, fieldName string) bool {
	// Handle special nested field cases
	switch fieldName {
	case "hostAliases":
		return w.hasContainerHostAliasesChanged(oldSpec, newSpec)
	case "securityContext":
		return w.hasSecurityContextChanged(oldSpec, newSpec)
	default:
		oldValue := oldSpec[fieldName]
		newValue := newSpec[fieldName]
		return !reflect.DeepEqual(oldValue, newValue)
	}
}

// hasContainerHostAliasesChanged checks if container hostAliases have changed
func (w *ResourceWatcher) hasContainerHostAliasesChanged(oldSpec, newSpec map[string]interface{}) bool {
	oldHostAliases := oldSpec["hostAliases"]
	newHostAliases := newSpec["hostAliases"]

	return !reflect.DeepEqual(oldHostAliases, newHostAliases)
}

// hasSecurityContextChanged checks if security context has changed (pod or container level)
func (w *ResourceWatcher) hasSecurityContextChanged(oldSpec, newSpec map[string]interface{}) bool {
	oldPodSecurityContext := oldSpec["securityContext"]
	newPodSecurityContext := newSpec["securityContext"]
	if !reflect.DeepEqual(oldPodSecurityContext, newPodSecurityContext) {
		return true
	}

	oldContainers, oldExists := oldSpec["containers"].([]interface{})
	newContainers, newExists := newSpec["containers"].([]interface{})

	if !oldExists || !newExists || len(oldContainers) == 0 || len(newContainers) == 0 {
		return oldExists != newExists
	}

	oldContainer, oldOk := oldContainers[0].(map[string]interface{})
	newContainer, newOk := newContainers[0].(map[string]interface{})

	if !oldOk || !newOk {
		return oldOk != newOk
	}

	oldContainerSecurityContext := oldContainer["securityContext"]
	newContainerSecurityContext := newContainer["securityContext"]

	return !reflect.DeepEqual(oldContainerSecurityContext, newContainerSecurityContext)
}

type ResourceEvent struct {
	Type            watch.EventType
	ResourceKind    string
	ResourceName    string
	Namespace       string
	User            string
	Timestamp       time.Time
	ResourceVersion string
	Priority        EventPriority
	CorrelationID   string
}

type ResourceWatchState struct {
	LastResourceVersion string
	ResourceVersionInfo *ResourceVersionInfo
	InitialResources    map[string]resourceInfo
	ReconnectCount      int64
	IsInitialized       bool
	InitializedTime     time.Time
	LastSuccessfulWatch time.Time
	ConsecutiveFailures int64
	LastHeartbeat       time.Time
	ConnectionHealthy   bool
	WatchInterface      watch.Interface
}

type resourceInfo struct {
	version  string
	lastSeen time.Time
	object   *unstructured.Unstructured
}

type ResourceVersionInfo struct {
	Version   string
	Timestamp time.Time
	IsStale   bool
}

func NewResourceWatcher(config *config.Config, notifier notifier.Notifier) (*ResourceWatcher, error) {
	// Load kubeconfig
	var kubeconfig *rest.Config
	var err error

	kubeconfig, err = rest.InClusterConfig()
	if err != nil {
		kubeconfig, err = clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig: %v", err)
		}
	}

	kubeconfig.QPS = 2.0
	kubeconfig.Burst = 3
	kubeconfig.Timeout = 0

	kubeconfig.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		if t, ok := rt.(*http.Transport); ok {
			t.MaxIdleConns = 20
			t.MaxIdleConnsPerHost = 5
			t.IdleConnTimeout = 90 * time.Second
			t.TLSHandshakeTimeout = 10 * time.Second
			t.ExpectContinueTimeout = 1 * time.Second
			t.ResponseHeaderTimeout = 30 * time.Second
			t.DisableCompression = false
			t.ForceAttemptHTTP2 = true
			return t
		}
		return rt
	}

	client, err := dynamic.NewForConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	bufferSize := 100
	if config.Watcher.EventBufferSize > 0 {
		bufferSize = config.Watcher.EventBufferSize
	}

	semaphoreCapacity := 5
	if config.Watcher.MaxConcurrentWatches > 0 {
		semaphoreCapacity = config.Watcher.MaxConcurrentWatches
	}

	watcher := &ResourceWatcher{
		client:      client,
		config:      config,
		notifier:    notifier,
		watchers:    make(map[string]*ResourceWatchState),
		metrics:     &WatcherMetrics{},
		ctx:         ctx,
		cancel:      cancel,
		mu:          sync.RWMutex{},
		semaphore:   make(chan struct{}, semaphoreCapacity),
		eventBuffer: make(chan *ResourceEvent, bufferSize),

		frequencyTracker: newEventFrequencyTracker(),
		adaptiveCooldown: newAdaptiveCooldown(10 * time.Second),
		eventCorrelator:  newEventCorrelator(),
		priorityQueue:    newPriorityQueue(),
	}

	watcher.eventHandler = func(event *ResourceEvent) {
		log.Printf("Resource event: %s %s/%s by %s", event.Type, event.Namespace, event.ResourceName, event.User)
	}

	return watcher, nil
}

func (w *ResourceWatcher) startEventProcessor() {
	go func() {
		batchInterval := 5 * time.Second
		if w.config.Watcher.EventBatchIntervalMs > 0 {
			batchInterval = time.Duration(w.config.Watcher.EventBatchIntervalMs) * time.Millisecond
		}

		ticker := time.NewTicker(batchInterval)
		defer ticker.Stop()

		var events []*ResourceEvent
		dedupMap := make(map[string]*ResourceEvent)
		lastNotificationTime := make(map[string]time.Time)
		notificationCooldown := 30 * time.Second
		if w.config.Watcher.NotificationCooldown > 0 {
			notificationCooldown = time.Duration(w.config.Watcher.NotificationCooldown) * time.Second
		}

		maxBatchSize := 50
		minBatchInterval := 1 * time.Second
		maxBatchInterval := 30 * time.Second
		currentBatchInterval := batchInterval

		cleanupTicker := time.NewTicker(5 * time.Minute)
		defer cleanupTicker.Stop()

		for {
			select {
			case <-w.ctx.Done():
				return
			case event := <-w.eventBuffer:
				key := fmt.Sprintf("%s:%s:%s:%s", event.ResourceKind, event.Namespace, event.ResourceName, event.Type)
				dedupMap[key] = event

				if len(dedupMap) >= maxBatchSize {
					events = make([]*ResourceEvent, 0, len(dedupMap))
					for _, event := range dedupMap {
						events = append(events, event)
					}

					for k := range dedupMap {
						delete(dedupMap, k)
					}

					w.processEventBatchEnhanced(events, lastNotificationTime, notificationCooldown)

					ticker.Reset(currentBatchInterval)
				}

			case <-ticker.C:
				if len(dedupMap) > 0 {
					events = make([]*ResourceEvent, 0, len(dedupMap))
					for _, event := range dedupMap {
						events = append(events, event)
					}

					for k := range dedupMap {
						delete(dedupMap, k)
					}

					w.processEventBatchEnhanced(events, lastNotificationTime, notificationCooldown)

					if len(events) > 20 {
						currentBatchInterval = minBatchInterval
					} else if len(events) < 5 {
						currentBatchInterval = maxBatchInterval
					} else {
						currentBatchInterval = batchInterval
					}
					ticker.Reset(currentBatchInterval)
				}

			case <-cleanupTicker.C:
				// Clean up old notification tracking entries
				w.cleanupOldNotificationEntries(lastNotificationTime, notificationCooldown)
			}
		}
	}()
}

func (w *ResourceWatcher) processEventBatch(events []*ResourceEvent, lastNotificationTime map[string]time.Time, notificationCooldown time.Duration) {
	w.processEventBatchEnhanced(events, lastNotificationTime, notificationCooldown)
}

func (w *ResourceWatcher) processEventBatchEnhanced(events []*ResourceEvent, lastNotificationTime map[string]time.Time, notificationCooldown time.Duration) {
	if len(events) == 0 {
		return
	}

	log.Printf("Processing enhanced batch of %d events", len(events))

	highPriority := make([]*ResourceEvent, 0)
	mediumPriority := make([]*ResourceEvent, 0)
	lowPriority := make([]*ResourceEvent, 0)

	for _, event := range events {
		switch event.Priority {
		case PriorityHigh:
			highPriority = append(highPriority, event)
		case PriorityMedium:
			mediumPriority = append(mediumPriority, event)
		case PriorityLow:
			lowPriority = append(lowPriority, event)
		}
	}

	notificationsSent := 0
	correlatedEvents := make(map[string][]*ResourceEvent)

	// Group events by correlation
	for _, event := range events {
		correlationKey := event.CorrelationID
		correlatedEvents[correlationKey] = append(correlatedEvents[correlationKey], event)
	}

	// Process correlated events
	for _, correlatedGroup := range correlatedEvents {
		if len(correlatedGroup) == 1 {
			event := correlatedGroup[0]
			notificationsSent += w.processSingleEvent(event, lastNotificationTime, notificationCooldown)
		} else {
			notificationsSent += w.processCorrelatedEvents(correlatedGroup, lastNotificationTime, notificationCooldown)
		}
	}

	w.metrics.EventsProcessed += int64(len(events))
	w.metrics.BufferUtilization = float64(len(w.eventBuffer)) / float64(cap(w.eventBuffer)) * 100

	if notificationsSent > 0 {
		log.Printf("Sent %d notifications from enhanced batch of %d events", notificationsSent, len(events))
	}
}

func (w *ResourceWatcher) processSingleEvent(event *ResourceEvent, lastNotificationTime map[string]time.Time, notificationCooldown time.Duration) int {
	if w.eventHandler != nil {
		w.eventHandler(event)
	}

	if w.notifier != nil {
		resourceKey := fmt.Sprintf("%s:%s:%s", event.ResourceKind, event.Namespace, event.ResourceName)
		now := time.Now()

		if lastTime, exists := lastNotificationTime[resourceKey]; !exists || now.Sub(lastTime) >= notificationCooldown {
			notificationEvent := notifier.NotificationEvent{
				EventType:    string(event.Type),
				ResourceKind: event.ResourceKind,
				ResourceName: event.ResourceName,
				Namespace:    event.Namespace,
				User:         event.User,
			}
			if err := w.notifier.SendNotification(notificationEvent); err != nil {
				log.Printf("Error sending notification for event %s %s/%s: %v",
					event.Type, event.Namespace, event.ResourceName, err)
				return 0
			} else {
				lastNotificationTime[resourceKey] = now
				return 1
			}
		} else {
			log.Printf("Skipping notification for %s %s/%s due to cooldown (last notification: %v ago)",
				event.Type, event.Namespace, event.ResourceName, now.Sub(lastTime))
		}
	}

	return 0
}

func (w *ResourceWatcher) processCorrelatedEvents(events []*ResourceEvent, lastNotificationTime map[string]time.Time, notificationCooldown time.Duration) int {
	if len(events) == 0 {
		return 0
	}

	baseEvent := events[0]
	resourceKey := fmt.Sprintf("%s:%s:%s", baseEvent.ResourceKind, baseEvent.Namespace, baseEvent.ResourceName)
	now := time.Now()

	// Check cooldown
	if lastTime, exists := lastNotificationTime[resourceKey]; exists && now.Sub(lastTime) < notificationCooldown {
		log.Printf("Skipping correlated notification for %s due to cooldown (last notification: %v ago)",
			resourceKey, now.Sub(lastTime))
		return 0
	}

	eventTypes := make(map[string]int)
	users := make(map[string]bool)

	for _, event := range events {
		eventTypes[string(event.Type)]++
		users[event.User] = true
	}

	var eventSummary []string
	for eventType, count := range eventTypes {
		if count == 1 {
			eventSummary = append(eventSummary, eventType)
		} else {
			eventSummary = append(eventSummary, fmt.Sprintf("%d %s", count, eventType))
		}
	}

	userList := make([]string, 0, len(users))
	for user := range users {
		userList = append(userList, user)
	}

	if w.notifier != nil {
		notificationEvent := notifier.NotificationEvent{
			EventType:    fmt.Sprintf("MULTIPLE: %s", strings.Join(eventSummary, ", ")),
			ResourceKind: baseEvent.ResourceKind,
			ResourceName: baseEvent.ResourceName,
			Namespace:    baseEvent.Namespace,
			User:         strings.Join(userList, ", "),
		}

		if err := w.notifier.SendNotification(notificationEvent); err != nil {
			log.Printf("Error sending correlated notification for %s: %v", resourceKey, err)
			return 0
		} else {
			lastNotificationTime[resourceKey] = now
			log.Printf("Sent consolidated notification for %s (%d correlated events)", resourceKey, len(events))
			return 1
		}
	}

	return 0
}

func (w *ResourceWatcher) isResourceVersionStale(resourceVersion string, watchState *ResourceWatchState) bool {
	if resourceVersion == "" {
		return true
	}

	ttl := time.Hour
	if w.config.Watcher.ResourceVersionTTL > 0 {
		ttl = time.Duration(w.config.Watcher.ResourceVersionTTL) * time.Second
	}

	if watchState.ResourceVersionInfo != nil {
		if time.Since(watchState.ResourceVersionInfo.Timestamp) > ttl {
			return true
		}
	}

	if watchState.ReconnectCount > 3 {
		return true
	}

	return false
}

func (w *ResourceWatcher) Start(startupCtx context.Context) error {
	w.startEventProcessor()

	for i, resource := range w.config.Resources {
		go func(resourceConfig config.ResourceConfig, index int) {
			if index > 0 {
				select {
				case <-time.After(time.Duration(index) * 2 * time.Second):
				case <-startupCtx.Done():
					log.Printf("Startup context cancelled before starting watcher for %s in namespace %s",
						resourceConfig.Kind, resourceConfig.Namespace)
					return
				}
			}

			select {
			case w.semaphore <- struct{}{}:
				defer func() { <-w.semaphore }() // Release semaphore when done
			case <-startupCtx.Done():
				log.Printf("Startup context cancelled before starting watcher for %s in namespace %s",
					resourceConfig.Kind, resourceConfig.Namespace)
				return
			}

			w.watchResource(w.ctx, resourceConfig, startupCtx)
		}(resource, i)
	}

	return nil
}

// Stop gracefully stops all watchers
func (w *ResourceWatcher) Stop() {
	w.cancel()

	// Clean up all watchers
	w.mu.Lock()
	defer w.mu.Unlock()

	for key, watchState := range w.watchers {
		if watchState.WatchInterface != nil {
			watchState.WatchInterface.Stop()
		}
		delete(w.watchers, key)
	}
}

// GetMetrics returns a copy of the current metrics
func (w *ResourceWatcher) GetMetrics() WatcherMetrics {
	return *w.metrics
}

// GetWatcherState returns the state of all watchers
func (w *ResourceWatcher) GetWatcherState() map[string]*ResourceWatchState {
	w.mu.RLock()
	defer w.mu.RUnlock()

	state := make(map[string]*ResourceWatchState)
	for key, watchState := range w.watchers {
		state[key] = watchState
	}
	return state
}

func (w *ResourceWatcher) watchResource(ctx context.Context, resourceConfig config.ResourceConfig, startupCtx context.Context) {
	// Create a unique key for this watcher
	watcherKey := fmt.Sprintf("%s/%s/%s", resourceConfig.Kind, resourceConfig.Namespace, resourceConfig.ResourceName)

	gvr := GetGroupVersionResource(resourceConfig.Kind)
	if gvr.Empty() {
		log.Printf("Error: Unknown resource kind %s", resourceConfig.Kind)
		return
	}

	if resourceConfig.ResourceName != "" {
		log.Printf("Starting to watch specific %s '%s' in namespace %s",
			resourceConfig.Kind, resourceConfig.ResourceName, resourceConfig.Namespace)
	} else {
		log.Printf("Starting to watch all %s in namespace %s",
			resourceConfig.Kind, resourceConfig.Namespace)
	}

	watchState := &ResourceWatchState{
		InitialResources:    make(map[string]resourceInfo),
		IsInitialized:       false,
		LastSuccessfulWatch: time.Now(),
		LastHeartbeat:       time.Now(),
		ConnectionHealthy:   false,
	}

	w.mu.Lock()
	w.watchers[watcherKey] = watchState
	w.mu.Unlock()

	// Ensure cleanup on exit
	defer func() {
		w.mu.Lock()
		if watchState.WatchInterface != nil {
			watchState.WatchInterface.Stop()
		}
		delete(w.watchers, watcherKey)
		w.mu.Unlock()
		log.Printf("Cleaned up watcher for %s", watcherKey)
	}()

	lastResourceVersion, err := w.loadInitialResourcesWithRetry(startupCtx, resourceConfig, gvr, watchState)
	if err != nil {
		log.Printf("Failed to load initial resources for %s in namespace %s after retries: %v",
			resourceConfig.Kind, resourceConfig.Namespace, err)
		return
	}

	watchState.IsInitialized = true
	watchState.InitializedTime = time.Now()
	watchStartTime := time.Now()

	log.Printf("Successfully loaded %d existing %s resources, starting watch with resource version: %s",
		len(watchState.InitialResources), resourceConfig.Kind, lastResourceVersion)

	w.startBackgroundGoroutines(ctx, resourceConfig, watchState)

	log.Printf("Waiting 10 seconds before starting to watch to avoid spam notifications...")
	startupDelay := 10 * time.Second
	startupTimer := time.NewTimer(startupDelay)
	defer startupTimer.Stop()

	select {
	case <-startupCtx.Done():
		log.Printf("Startup context cancelled during startup delay, stopping watch for %s in namespace %s",
			resourceConfig.Kind, resourceConfig.Namespace)
		return
	case <-ctx.Done():
		log.Printf("Runtime context cancelled during startup delay, stopping watch for %s in namespace %s",
			resourceConfig.Kind, resourceConfig.Namespace)
		return
	case <-startupTimer.C:
	}

	log.Printf("Starting watch at: %s", watchStartTime.Format(time.RFC3339))

	processedEvents := make(map[string]time.Time)

	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				log.Printf("Processed events cleanup goroutine exiting due to context cancellation for %s in namespace %s",
					resourceConfig.Kind, resourceConfig.Namespace)
				return
			case <-ticker.C:
				now := time.Now()
				for eventKey, timestamp := range processedEvents {
					if now.Sub(timestamp) > time.Hour {
						delete(processedEvents, eventKey)
					}
				}
			}
		}
	}()

	w.runWatchLoop(ctx, resourceConfig, gvr, watchState, lastResourceVersion, processedEvents, watchStartTime)
}

func (w *ResourceWatcher) loadInitialResourcesWithRetry(ctx context.Context, resourceConfig config.ResourceConfig, gvr schema.GroupVersionResource, watchState *ResourceWatchState) (string, error) {
	backoff := wait.Backoff{
		Steps:    10,
		Duration: 1 * time.Second,
		Factor:   2.0,
		Jitter:   0.1,
		Cap:      5 * time.Minute,
	}

	var lastResourceVersion string
	var err error

	for attempt := 1; attempt <= backoff.Steps; attempt++ {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("context cancelled during initial resource loading: %v", ctx.Err())
		default:
		}

		lastResourceVersion, err = w.loadInitialResources(ctx, resourceConfig, gvr, watchState)
		if err == nil {
			return lastResourceVersion, nil
		}

		log.Printf("Error loading initial resources (attempt %d/%d): %v", attempt, backoff.Steps, err)

		if attempt < backoff.Steps {
			// Wait before retrying
			backoffDuration := backoff.Step()
			log.Printf("Retrying initial resource loading in %v...", backoffDuration)
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("context cancelled during retry: %v", ctx.Err())
			case <-time.After(backoffDuration):
			}
		}
	}

	return "", fmt.Errorf("failed to load initial resources after %d attempts: %v", backoff.Steps, err)
}

func (w *ResourceWatcher) loadInitialResources(ctx context.Context, resourceConfig config.ResourceConfig, gvr schema.GroupVersionResource, watchState *ResourceWatchState) (string, error) {
	var listOptions metav1.ListOptions
	if resourceConfig.ResourceName != "" {
		listOptions = metav1.ListOptions{
			FieldSelector: fmt.Sprintf("metadata.name=%s", resourceConfig.ResourceName),
			Limit:         500,
		}
	} else {
		listOptions = metav1.ListOptions{
			Limit: 500,
		}
	}

	var lastResourceVersion string

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("context cancelled during initial resource loading: %v", ctx.Err())
		default:
		}

		listCtx, listCancel := context.WithTimeout(ctx, 30*time.Second)
		existingResources, err := w.client.Resource(gvr).Namespace(resourceConfig.Namespace).List(
			listCtx,
			listOptions,
		)
		listCancel()

		if err != nil {
			return "", fmt.Errorf("error listing existing %s in namespace %s: %v",
				resourceConfig.Kind, resourceConfig.Namespace, err)
		}

		for _, item := range existingResources.Items {
			metadata, err := meta.Accessor(&item)
			if err != nil {
				continue
			}
			key := fmt.Sprintf("%s/%s", metadata.GetNamespace(), metadata.GetName())
			watchState.InitialResources[key] = resourceInfo{
				version:  metadata.GetResourceVersion(),
				lastSeen: time.Now(),
				object:   item.DeepCopy(),
			}
			lastResourceVersion = metadata.GetResourceVersion()
		}

		if lastResourceVersion != "" {
			watchState.ResourceVersionInfo = &ResourceVersionInfo{
				Version:   lastResourceVersion,
				Timestamp: time.Now(),
				IsStale:   false,
			}
		}
		if existingResources.GetContinue() == "" {
			break
		}
		listOptions.Continue = existingResources.GetContinue()
	}

	return lastResourceVersion, nil
}

func (w *ResourceWatcher) startBackgroundGoroutines(ctx context.Context, resourceConfig config.ResourceConfig, watchState *ResourceWatchState) {
	// Keep-alive heartbeat goroutine
	heartbeatInterval := 30 * time.Second
	if w.config.Watcher.HeartbeatIntervalMs > 0 {
		heartbeatInterval = time.Duration(w.config.Watcher.HeartbeatIntervalMs) * time.Millisecond
	}

	keepAliveEnabled := true
	if w.config.Watcher.KeepAliveEnabled {
		keepAliveEnabled = w.config.Watcher.KeepAliveEnabled
	}

	if keepAliveEnabled {
		go func() {
			ticker := time.NewTicker(heartbeatInterval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					log.Printf("Keep-alive goroutine exiting due to context cancellation for %s in namespace %s",
						resourceConfig.Kind, resourceConfig.Namespace)
					return
				case <-ticker.C:
					if watchState.WatchInterface != nil && watchState.ConnectionHealthy {
						timeSinceLastHeartbeat := time.Since(watchState.LastHeartbeat)
						if timeSinceLastHeartbeat > heartbeatInterval*3 {
							log.Printf("Keep-alive: No events received for %v, checking connection health", timeSinceLastHeartbeat)
							watchState.ConnectionHealthy = false
						}
					}
				}
			}
		}()
	}

	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				log.Printf("Cleanup goroutine exiting due to context cancellation for %s in namespace %s",
					resourceConfig.Kind, resourceConfig.Namespace)
				return
			case <-ticker.C:
				now := time.Now()
				for key, info := range watchState.InitialResources {
					if now.Sub(info.lastSeen) > 24*time.Hour {
						delete(watchState.InitialResources, key)
					}
				}
			}
		}
	}()
}

func (w *ResourceWatcher) runWatchLoop(ctx context.Context, resourceConfig config.ResourceConfig, gvr schema.GroupVersionResource, watchState *ResourceWatchState, lastResourceVersion string, processedEvents map[string]time.Time, watchStartTime time.Time) {
	backoff := wait.Backoff{
		Steps:    10,
		Duration: 1 * time.Second,
		Factor:   2.0,
		Jitter:   0.1,
		Cap:      5 * time.Minute,
	}

	for {
		select {
		case <-ctx.Done():
			log.Printf("Context cancelled, stopping watch for %s in namespace %s", resourceConfig.Kind, resourceConfig.Namespace)
			return
		default:
		}

		if watchState.WatchInterface != nil && !watchState.ConnectionHealthy {
			log.Printf("Keep-alive: Detected stale connection, forcing reconnection")
			watchState.WatchInterface.Stop()
			watchState.WatchInterface = nil
		}

		timeoutSeconds := int64(300)
		if w.config.Watcher.WatchTimeoutSeconds > 0 {
			timeoutSeconds = w.config.Watcher.WatchTimeoutSeconds
		}

		if w.isResourceVersionStale(lastResourceVersion, watchState) {
			log.Printf("Resource version appears stale for %s in namespace %s, starting fresh",
				resourceConfig.Kind, resourceConfig.Namespace)
			lastResourceVersion = ""
			watchState.ReconnectCount = 0
			watchState.ConsecutiveFailures = 0
			watchState.ResourceVersionInfo = nil
		}

		watchOptions := metav1.ListOptions{
			TimeoutSeconds:      &timeoutSeconds,
			ResourceVersion:     lastResourceVersion,
			AllowWatchBookmarks: true,
		}

		if lastResourceVersion == "" {
			log.Printf("Starting fresh watch for %s in namespace %s (no resource version)",
				resourceConfig.Kind, resourceConfig.Namespace)
		}

		if resourceConfig.ResourceName != "" {
			watchOptions.FieldSelector = fmt.Sprintf("metadata.name=%s", resourceConfig.ResourceName)
		}

		watch, watchErr := w.client.Resource(gvr).Namespace(resourceConfig.Namespace).Watch(ctx, watchOptions)
		if watchErr != nil {
			if ctx.Err() != nil {
				log.Printf("Context was cancelled during watch creation, stopping watch for %s in namespace %s",
					resourceConfig.Kind, resourceConfig.Namespace)
				return
			}

			log.Printf("Error watching %s in namespace %s: %v",
				resourceConfig.Kind, resourceConfig.Namespace, watchErr)
			w.metrics.WatchErrors++
			watchState.ConsecutiveFailures++
			watchState.ConnectionHealthy = false

			if strings.Contains(watchErr.Error(), "too old resource version") {
				log.Printf("Resource version too old for %s in namespace %s, forcing complete reset",
					resourceConfig.Kind, resourceConfig.Namespace)
				lastResourceVersion = ""
				watchState.ReconnectCount = 0
				watchState.InitialResources = make(map[string]resourceInfo)
				watchState.IsInitialized = false
				watchState.ConsecutiveFailures = 0
				watchState.ResourceVersionInfo = nil
				time.Sleep(500 * time.Millisecond)
				continue
			}

			backoffDuration := backoff.Step()
			log.Printf("Retrying in %v...", backoffDuration)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoffDuration):
			}
			continue
		}

		watchState.ConsecutiveFailures = 0
		watchState.LastSuccessfulWatch = time.Now()
		watchState.ConnectionHealthy = true
		watchState.WatchInterface = watch
		watchState.LastHeartbeat = time.Now()

		log.Printf("Watch connection established successfully for %s in namespace %s",
			resourceConfig.Kind, resourceConfig.Namespace)

		w.processWatchEvents(ctx, resourceConfig, watch, watchState, lastResourceVersion, processedEvents, watchStartTime)

		watchState.ReconnectCount++
		w.metrics.WatchReconnects++
		watchState.ConnectionHealthy = false

		if watchState.WatchInterface != nil {
			watchState.WatchInterface.Stop()
			watchState.WatchInterface = nil
		}

		timeSinceLastSuccess := time.Since(watchState.LastSuccessfulWatch)
		timeSinceLastHeartbeat := time.Since(watchState.LastHeartbeat)
		log.Printf("Watch ended for %s in namespace %s (reconnect #%d, last success: %v ago, last heartbeat: %v ago)",
			resourceConfig.Kind, resourceConfig.Namespace, watchState.ReconnectCount,
			timeSinceLastSuccess, timeSinceLastHeartbeat)

		// Handle reconnection
		if watchState.ReconnectCount > 5 {
			log.Printf("Too many reconnects (%d), resetting state", watchState.ReconnectCount)
			lastResourceVersion = ""
			watchState.InitialResources = make(map[string]resourceInfo)
			watchState.IsInitialized = false
			watchState.ReconnectCount = 0
			watchState.ConsecutiveFailures = 0
			processedEvents = make(map[string]time.Time)
			time.Sleep(30 * time.Second)
			continue
		}

		backoffDuration := time.Duration(watchState.ReconnectCount*5) * time.Second
		if backoffDuration > 30*time.Second {
			backoffDuration = 30 * time.Second
		}

		log.Printf("Waiting %v before reconnecting...", backoffDuration)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoffDuration):
		}
	}
}

func GetGroupVersionResource(kind string) schema.GroupVersionResource {
	switch kind {
	case "ConfigMap":
		return schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "configmaps",
		}
	case "Secret":
		return schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "secrets",
		}
	case "Deployment":
		return schema.GroupVersionResource{
			Group:    "apps",
			Version:  "v1",
			Resource: "deployments",
		}
	case "Ingress":
		return schema.GroupVersionResource{
			Group:    "networking.k8s.io",
			Version:  "v1",
			Resource: "ingresses",
		}
	default:
		return schema.GroupVersionResource{}
	}
}

func (w *ResourceWatcher) processWatchEvents(ctx context.Context, resourceConfig config.ResourceConfig, watch watch.Interface, watchState *ResourceWatchState, lastResourceVersion string, processedEvents map[string]time.Time, watchStartTime time.Time) {
	for event := range watch.ResultChan() {
		select {
		case <-ctx.Done():
			log.Printf("Context cancelled during event processing, stopping watch for %s in namespace %s",
				resourceConfig.Kind, resourceConfig.Namespace)
			return
		default:
		}

		// Update heartbeat on any event
		watchState.LastHeartbeat = time.Now()
		watchState.ConnectionHealthy = true

		if status, ok := event.Object.(*metav1.Status); ok {
			log.Printf("Received status event: %s", status.Status)
			if status.Status == "Failure" {
				log.Printf("Watch failed: %s", status.Message)
				break
			}
			continue
		}

		if event.Type == "BOOKMARK" {
			log.Printf("Received bookmark event, updating resource version")
			if obj, ok := event.Object.(*unstructured.Unstructured); ok {
				lastResourceVersion = obj.GetResourceVersion()
				watchState.ResourceVersionInfo = &ResourceVersionInfo{
					Version:   lastResourceVersion,
					Timestamp: time.Now(),
					IsStale:   false,
				}
			}
			continue
		}

		obj, ok := event.Object.(*unstructured.Unstructured)
		if !ok {
			log.Printf("Warning: unexpected object type: %T, skipping event", event.Object)
			continue
		}

		metadata, err := meta.Accessor(obj)
		if err != nil {
			log.Printf("Error accessing object metadata: %v", err)
			continue
		}

		if resourceConfig.ResourceName != "" && metadata.GetName() != resourceConfig.ResourceName {
			continue
		}

		lastResourceVersion = metadata.GetResourceVersion()
		watchState.ResourceVersionInfo = &ResourceVersionInfo{
			Version:   lastResourceVersion,
			Timestamp: time.Now(),
			IsStale:   false,
		}

		user := "unknown"
		if annotations := metadata.GetAnnotations(); annotations != nil {
			if u, ok := annotations["kubernetes.io/change-cause"]; ok {
				user = u
			} else if u, ok := annotations["kubectl.kubernetes.io/last-applied-configuration"]; ok {
				if strings.Contains(u, "kubectl") {
					user = "kubectl"
				}
			}
		}
		if labels := metadata.GetLabels(); labels != nil {
			if u, ok := labels["app.kubernetes.io/created-by"]; ok {
				user = u
			}
		}

		resourceEvent := &ResourceEvent{
			Type:            event.Type,
			ResourceKind:    resourceConfig.Kind,
			ResourceName:    metadata.GetName(),
			Namespace:       metadata.GetNamespace(),
			User:            user,
			Timestamp:       time.Now(),
			ResourceVersion: metadata.GetResourceVersion(),
			Priority:        determineEventPriority(event.Type, resourceConfig.Kind),
			CorrelationID: generateCorrelationID(&ResourceEvent{
				ResourceKind: resourceConfig.Kind,
				Namespace:    metadata.GetNamespace(),
				ResourceName: metadata.GetName(),
				User:         user,
			}),
		}

		eventKey := fmt.Sprintf("%s:%s:%s:%s:%s:%d",
			event.Type,
			resourceConfig.Kind,
			metadata.GetNamespace(),
			metadata.GetName(),
			metadata.GetResourceVersion(),
			watchState.ReconnectCount)

		if _, exists := processedEvents[eventKey]; exists {
			log.Printf("Skipping duplicate event: %s", eventKey)
			w.metrics.EventsSkipped++
			continue
		}

		processedEvents[eventKey] = time.Now()
		w.metrics.EventsReceived++

		key := fmt.Sprintf("%s/%s", metadata.GetNamespace(), metadata.GetName())
		_, isExisting := watchState.InitialResources[key]

		eventProcessed := false

		switch event.Type {
		case "ADDED":

			resourceCreationTime := metadata.GetCreationTimestamp()
			timeSinceWatchStart := time.Since(watchStartTime)

			if !isExisting {
				shouldNotify := false
				if !resourceCreationTime.IsZero() {
					timeSinceCreation := time.Since(resourceCreationTime.Time)
					if timeSinceCreation < timeSinceWatchStart+30*time.Second {
						shouldNotify = true
					}
				} else {
					shouldNotify = timeSinceWatchStart > 30*time.Second
				}

				if shouldNotify {
					log.Printf("[%s] New resource %s/%s was ADDED by %s",
						resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName(), user)
					select {
					case w.eventBuffer <- resourceEvent:
						eventProcessed = true
						w.metrics.EventsBatched++
					default:
						log.Printf("Event buffer full, dropping ADDED event for %s/%s", metadata.GetNamespace(), metadata.GetName())
						w.metrics.EventsDropped++
					}
				} else {
					log.Printf("[%s] Resource %s/%s discovered during initial load (no notification sent)",
						resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName())
				}
			}

			watchState.InitialResources[key] = resourceInfo{
				version:  metadata.GetResourceVersion(),
				lastSeen: time.Now(),
				object:   obj.DeepCopy(),
			}

		case "MODIFIED":
			if timeSinceStart := time.Since(watchStartTime); timeSinceStart > 30*time.Second {
				if existingInfo, exists := watchState.InitialResources[key]; exists {
					if existingInfo.version != metadata.GetResourceVersion() {
						shouldNotify := true
						if existingInfo.object != nil {
							// Store the old object for comparison before updating it
							oldObject := existingInfo.object
							shouldNotify = w.shouldNotifyForChange(resourceConfig.Kind, oldObject, obj)
						}

						if shouldNotify {
							resourceKey := fmt.Sprintf("%s:%s:%s", resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName())
							w.frequencyTracker.recordEvent(resourceKey)
							eventFrequency := w.frequencyTracker.getEventFrequency(resourceKey)

							baseCooldown := 10 * time.Second
							if w.config.Watcher.ModificationCooldown > 0 {
								baseCooldown = time.Duration(w.config.Watcher.ModificationCooldown) * time.Second
							}
							w.adaptiveCooldown.baseCooldown = baseCooldown
							adaptiveCooldown := w.adaptiveCooldown.getCooldown(eventFrequency)

							timeSinceLastModification := time.Since(existingInfo.lastSeen)

							if timeSinceLastModification >= adaptiveCooldown {
								log.Printf("[%s] Resource %s/%s was MODIFIED by %s (version: %s -> %s, frequency: %.1f/min, cooldown: %v)",
									resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName(), user,
									existingInfo.version, metadata.GetResourceVersion(), eventFrequency, adaptiveCooldown)

								w.eventCorrelator.addEvent(resourceEvent)

								select {
								case w.eventBuffer <- resourceEvent:
									eventProcessed = true
									w.metrics.EventsBatched++
								default:
									log.Printf("Event buffer full, dropping MODIFIED event for %s/%s", metadata.GetNamespace(), metadata.GetName())
									w.metrics.EventsDropped++
								}
							} else {
								log.Printf("[%s] Skipping MODIFIED event for %s/%s (last modification was %v ago, adaptive cooldown: %v)",
									resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName(), timeSinceLastModification, adaptiveCooldown)
								w.metrics.EventsSkipped++
							}
						} else {
							log.Printf("[%s] Resource %s/%s MODIFIED but no important fields changed - likely status change (pod restart)",
								resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName())
							w.metrics.EventsSkipped++
						}
					}
				} else {
					log.Printf("[%s] Resource %s/%s was MODIFIED (new resource) by %s",
						resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName(), user)

					w.eventCorrelator.addEvent(resourceEvent)

					select {
					case w.eventBuffer <- resourceEvent:
						eventProcessed = true
						w.metrics.EventsBatched++
					default:
						log.Printf("Event buffer full, dropping MODIFIED event for %s/%s", metadata.GetNamespace(), metadata.GetName())
						w.metrics.EventsDropped++
					}
				}
			}

			// Update the stored object AFTER processing the event
			watchState.InitialResources[key] = resourceInfo{
				version:  metadata.GetResourceVersion(),
				lastSeen: time.Now(),
				object:   obj.DeepCopy(),
			}

		case "DELETED":
			if timeSinceStart := time.Since(watchStartTime); timeSinceStart > 30*time.Second {
				log.Printf("[%s] Resource %s/%s was DELETED by %s",
					resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName(), user)
				select {
				case w.eventBuffer <- resourceEvent:
					eventProcessed = true
					w.metrics.EventsBatched++
				default:
					log.Printf("Event buffer full, dropping DELETED event for %s/%s", metadata.GetNamespace(), metadata.GetName())
					w.metrics.EventsDropped++
				}
			}
			delete(watchState.InitialResources, key)

		default:
			log.Printf("Skipping unknown event type: %s", event.Type)
			w.metrics.EventsSkipped++
			continue
		}
		if eventProcessed {
			w.eventHandler(resourceEvent)
		}
	}
}
