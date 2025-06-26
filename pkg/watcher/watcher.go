package watcher

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
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
	semaphore    chan struct{} // Limit concurrent watchers
}

// WatcherMetrics tracks watcher statistics
type WatcherMetrics struct {
	EventsReceived  int64
	EventsProcessed int64
	EventsSkipped   int64
	WatchErrors     int64
	WatchReconnects int64
}

// ResourceEvent represents a Kubernetes resource event
type ResourceEvent struct {
	Type            watch.EventType
	ResourceKind    string
	ResourceName    string
	Namespace       string
	User            string
	Timestamp       time.Time
	ResourceVersion string
}

// ResourceWatchState tracks the state for each resource being watched
type ResourceWatchState struct {
	LastResourceVersion string
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
}

// NewResourceWatcher creates a new watcher instance
func NewResourceWatcher(config *config.Config, notifier notifier.Notifier) (*ResourceWatcher, error) {
	// Load kubeconfig
	var kubeconfig *rest.Config
	var err error

	// Try in-cluster config first, then fall back to kubeconfig path
	kubeconfig, err = rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig from environment
		kubeconfig, err = clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig: %v", err)
		}
	}

	// Apply very conservative client configuration to reduce connection issues
	kubeconfig.QPS = 2.0   // Extremely conservative QPS
	kubeconfig.Burst = 3   // Minimal burst
	kubeconfig.Timeout = 0 // No client-side timeout, let server control

	// Configure transport settings
	kubeconfig.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		if t, ok := rt.(*http.Transport); ok {
			t.MaxIdleConns = 10
			t.MaxIdleConnsPerHost = 2
			t.IdleConnTimeout = 60 * time.Second
			t.TLSHandshakeTimeout = 10 * time.Second
			t.ExpectContinueTimeout = 1 * time.Second
			t.ResponseHeaderTimeout = 30 * time.Second
			return t
		}
		return rt
	}

	// Create dynamic client
	client, err := dynamic.NewForConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %v", err)
	}

	// Create context for the watcher
	ctx, cancel := context.WithCancel(context.Background())

	// Create watcher with conservative settings
	watcher := &ResourceWatcher{
		client:    client,
		config:    config,
		notifier:  notifier,
		watchers:  make(map[string]*ResourceWatchState),
		metrics:   &WatcherMetrics{},
		ctx:       ctx,
		cancel:    cancel,
		mu:        sync.RWMutex{},
		semaphore: make(chan struct{}, 2), // Even more conservative: limit to 2 concurrent watchers
	}

	// Set default event handler
	watcher.eventHandler = func(event *ResourceEvent) {
		log.Printf("Resource event: %s %s/%s by %s", event.Type, event.Namespace, event.ResourceName, event.User)
	}

	return watcher, nil
}

// Start begins watching the configured resources
func (w *ResourceWatcher) Start(startupCtx context.Context) error {
	// Start watching each resource in its own goroutine
	// Use the main context (w.ctx) for runtime operations, not the startup context
	for _, resource := range w.config.Resources {
		go func(resourceConfig config.ResourceConfig) {
			// Acquire semaphore to limit concurrent watchers
			select {
			case w.semaphore <- struct{}{}:
				defer func() { <-w.semaphore }() // Release semaphore when done
			case <-startupCtx.Done():
				log.Printf("Startup context cancelled before starting watcher for %s in namespace %s",
					resourceConfig.Kind, resourceConfig.Namespace)
				return
			}

			w.watchResource(w.ctx, resourceConfig, startupCtx)
		}(resource)
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

	// Get the GVR for the resource kind
	gvr := GetGroupVersionResource(resourceConfig.Kind)
	if gvr.Empty() {
		log.Printf("Error: Unknown resource kind %s", resourceConfig.Kind)
		return
	}

	// Log what we're watching
	if resourceConfig.ResourceName != "" {
		log.Printf("Starting to watch specific %s '%s' in namespace %s",
			resourceConfig.Kind, resourceConfig.ResourceName, resourceConfig.Namespace)
	} else {
		log.Printf("Starting to watch all %s in namespace %s",
			resourceConfig.Kind, resourceConfig.Namespace)
	}

	// Initialize resource watch state
	watchState := &ResourceWatchState{
		InitialResources:    make(map[string]resourceInfo),
		IsInitialized:       false,
		LastSuccessfulWatch: time.Now(),
		LastHeartbeat:       time.Now(),
		ConnectionHealthy:   false,
	}

	// Register this watcher
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

	// CRITICAL: Load initial resources with retry logic
	// This must succeed before starting the watch to ensure event continuity
	lastResourceVersion, err := w.loadInitialResourcesWithRetry(startupCtx, resourceConfig, gvr, watchState)
	if err != nil {
		log.Printf("Failed to load initial resources for %s in namespace %s after retries: %v",
			resourceConfig.Kind, resourceConfig.Namespace, err)
		return
	}

	// Mark as initialized and set the watch start time
	watchState.IsInitialized = true
	watchState.InitializedTime = time.Now()
	watchStartTime := time.Now()

	log.Printf("Successfully loaded %d existing %s resources, starting watch with resource version: %s",
		len(watchState.InitialResources), resourceConfig.Kind, lastResourceVersion)

	// Start background goroutines for cleanup and keep-alive
	w.startBackgroundGoroutines(ctx, resourceConfig, watchState)

	// Add a startup delay to avoid spam notifications
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
		// Continue with normal operation
	}

	// Mark the actual watch start time
	log.Printf("Starting watch at: %s", watchStartTime.Format(time.RFC3339))

	// Track processed events to prevent duplicates
	processedEvents := make(map[string]time.Time)

	// Cleanup processed events periodically
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
					// Remove events older than 1 hour
					if now.Sub(timestamp) > time.Hour {
						delete(processedEvents, eventKey)
					}
				}
			}
		}
	}()

	// Main watch loop - use a single context for the entire watch lifecycle
	w.runWatchLoop(ctx, resourceConfig, gvr, watchState, lastResourceVersion, processedEvents, watchStartTime)
}

func (w *ResourceWatcher) loadInitialResourcesWithRetry(ctx context.Context, resourceConfig config.ResourceConfig, gvr schema.GroupVersionResource, watchState *ResourceWatchState) (string, error) {
	// Create a backoff for retries with exponential backoff
	backoff := wait.Backoff{
		Steps:    10,
		Duration: 1 * time.Second,
		Factor:   2.0,
		Jitter:   0.1,
		Cap:      5 * time.Minute,
	}

	// Load initial resources with retry logic
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
			// Success
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

	// List existing resources with pagination
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("context cancelled during initial resource loading: %v", ctx.Err())
		default:
		}

		// Create a timeout context for the list operation
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

		// Store resource versions of existing resources
		for _, item := range existingResources.Items {
			metadata, err := meta.Accessor(&item)
			if err != nil {
				continue
			}
			key := fmt.Sprintf("%s/%s", metadata.GetNamespace(), metadata.GetName())
			watchState.InitialResources[key] = resourceInfo{
				version:  metadata.GetResourceVersion(),
				lastSeen: time.Now(),
			}
			lastResourceVersion = metadata.GetResourceVersion()
		}

		// Check if we need to continue pagination
		if existingResources.GetContinue() == "" {
			break
		}
		listOptions.Continue = existingResources.GetContinue()
	}

	return lastResourceVersion, nil
}

func (w *ResourceWatcher) startBackgroundGoroutines(ctx context.Context, resourceConfig config.ResourceConfig, watchState *ResourceWatchState) {
	// Keep-alive heartbeat goroutine
	heartbeatInterval := 30 * time.Second // Default 30 seconds
	if w.config.Watcher.HeartbeatIntervalMs > 0 {
		heartbeatInterval = time.Duration(w.config.Watcher.HeartbeatIntervalMs) * time.Millisecond
	}

	// Safe access to keep-alive setting with default
	keepAliveEnabled := true // Default to true
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
					// Check if we have an active watch and it's been too long since last event
					if watchState.WatchInterface != nil && watchState.ConnectionHealthy {
						timeSinceLastHeartbeat := time.Since(watchState.LastHeartbeat)
						if timeSinceLastHeartbeat > heartbeatInterval*3 {
							log.Printf("Keep-alive: No events received for %v, checking connection health", timeSinceLastHeartbeat)
							// Force a reconnection if we haven't received events for too long
							watchState.ConnectionHealthy = false
						}
					}
				}
			}
		}()
	}

	// Cleanup goroutine for the initialResources map
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
					// Remove entries older than 24 hours
					if now.Sub(info.lastSeen) > 24*time.Hour {
						delete(watchState.InitialResources, key)
					}
				}
			}
		}
	}()
}

func (w *ResourceWatcher) runWatchLoop(ctx context.Context, resourceConfig config.ResourceConfig, gvr schema.GroupVersionResource, watchState *ResourceWatchState, lastResourceVersion string, processedEvents map[string]time.Time, watchStartTime time.Time) {
	// Create a backoff for retries with exponential backoff
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

		// Check if keep-alive detected a stale connection
		if watchState.WatchInterface != nil && !watchState.ConnectionHealthy {
			log.Printf("Keep-alive: Detected stale connection, forcing reconnection")
			watchState.WatchInterface.Stop()
			watchState.WatchInterface = nil
		}

		// Get timeout from config or use default
		timeoutSeconds := int64(300) // Default 5 minutes
		if w.config.Watcher.WatchTimeoutSeconds > 0 {
			timeoutSeconds = w.config.Watcher.WatchTimeoutSeconds
		}

		// Create watch options with more resilient settings
		watchOptions := metav1.ListOptions{
			TimeoutSeconds: &timeoutSeconds,
			// Allow at least 30 seconds for the server to establish the watch
			ResourceVersion:     lastResourceVersion,
			AllowWatchBookmarks: true,
		}

		if resourceConfig.ResourceName != "" {
			watchOptions.FieldSelector = fmt.Sprintf("metadata.name=%s", resourceConfig.ResourceName)
		}

		// Create the watch
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

			// Handle specific error cases
			if strings.Contains(watchErr.Error(), "too old resource version") {
				log.Printf("Resource version too old, forcing complete reset")
				lastResourceVersion = ""
				watchState.ReconnectCount = 0
				watchState.InitialResources = make(map[string]resourceInfo)
				watchState.IsInitialized = false
				watchState.ConsecutiveFailures = 0
				// Don't wait long for resource version errors
				time.Sleep(1 * time.Second)
				continue
			}

			// Use exponential backoff for other errors
			backoffDuration := backoff.Step()
			log.Printf("Retrying in %v...", backoffDuration)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoffDuration):
			}
			continue
		}

		// Reset consecutive failures on successful watch creation
		watchState.ConsecutiveFailures = 0
		watchState.LastSuccessfulWatch = time.Now()
		watchState.ConnectionHealthy = true
		watchState.WatchInterface = watch
		watchState.LastHeartbeat = time.Now()

		log.Printf("Watch connection established successfully for %s in namespace %s",
			resourceConfig.Kind, resourceConfig.Namespace)

		// Process events
		w.processWatchEvents(ctx, resourceConfig, watch, watchState, lastResourceVersion, processedEvents, watchStartTime)

		// If we get here, the watch has ended
		watchState.ReconnectCount++
		w.metrics.WatchReconnects++
		watchState.ConnectionHealthy = false

		// Ensure the watch interface is properly closed
		if watchState.WatchInterface != nil {
			watchState.WatchInterface.Stop()
			watchState.WatchInterface = nil
		}

		// Log reconnection attempt
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
			// Wait longer before trying again
			time.Sleep(30 * time.Second)
			continue
		}

		// Calculate backoff duration based on reconnect count
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

// GetGroupVersionResource returns the GroupVersionResource for a given resource kind
func GetGroupVersionResource(kind string) schema.GroupVersionResource {
	// Map common resource kinds to their GVR
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
	// Add more resource types as needed
	default:
		return schema.GroupVersionResource{}
	}
}

func (w *ResourceWatcher) processWatchEvents(ctx context.Context, resourceConfig config.ResourceConfig, watch watch.Interface, watchState *ResourceWatchState, lastResourceVersion string, processedEvents map[string]time.Time, watchStartTime time.Time) {
	// Process events
	for event := range watch.ResultChan() {
		// Check for context cancellation before processing each event
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

		// Handle Status objects and other unexpected types
		if status, ok := event.Object.(*metav1.Status); ok {
			log.Printf("Received status event: %s", status.Status)
			if status.Status == "Failure" {
				log.Printf("Watch failed: %s", status.Message)

				// Check if it's a resource version error
				if strings.Contains(status.Message, "too old resource version") {
					log.Printf("Resource version too old, forcing complete reset")
					// Force complete reset - don't use any resource version
					lastResourceVersion = ""
					watchState.ReconnectCount = 0
					watchState.InitialResources = make(map[string]resourceInfo)
					watchState.IsInitialized = false
					watchState.ConsecutiveFailures = 0
					watchState.ConnectionHealthy = false
					// Don't try to reload initial resources - start completely fresh
					log.Printf("Starting watch without resource version")
				} else if strings.Contains(status.Message, "context canceled") ||
					strings.Contains(status.Message, "unable to decode an event from the watch stream") {
					log.Printf("API server context cancellation detected in status event")
					// For API server cancellations, wait longer before retrying
					backoffDuration := time.Duration(watchState.ConsecutiveFailures+1) * 30 * time.Second
					if backoffDuration > 5*time.Minute {
						backoffDuration = 5 * time.Minute
					}
					log.Printf("Waiting %v before retry due to API server cancellation...", backoffDuration)
					select {
					case <-ctx.Done():
						log.Printf("Context cancelled during API server cancellation backoff, stopping watch for %s in namespace %s",
							resourceConfig.Kind, resourceConfig.Namespace)
						return
					case <-time.After(backoffDuration):
					}
				}
				break
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

		// Skip if watching specific resource and this isn't it
		if resourceConfig.ResourceName != "" && metadata.GetName() != resourceConfig.ResourceName {
			continue
		}

		// Update last resource version
		lastResourceVersion = metadata.GetResourceVersion()

		// Get the user who made the change
		user := "unknown"
		if annotations := metadata.GetAnnotations(); annotations != nil {
			// Check multiple possible sources for user information
			if u, ok := annotations["kubernetes.io/change-cause"]; ok {
				user = u
			} else if u, ok := annotations["kubectl.kubernetes.io/last-applied-configuration"]; ok {
				// Try to extract user from kubectl apply
				if strings.Contains(u, "kubectl") {
					user = "kubectl"
				}
			}
		}
		// Check labels as well
		if labels := metadata.GetLabels(); labels != nil {
			if u, ok := labels["app.kubernetes.io/created-by"]; ok {
				user = u
			}
		}

		// Create resource event
		resourceEvent := &ResourceEvent{
			Type:            event.Type,
			ResourceKind:    resourceConfig.Kind,
			ResourceName:    metadata.GetName(),
			Namespace:       metadata.GetNamespace(),
			User:            user,
			Timestamp:       time.Now(),
			ResourceVersion: metadata.GetResourceVersion(),
		}

		// Create unique event key to prevent duplicates
		// Include more fields to ensure uniqueness across reconnections
		eventKey := fmt.Sprintf("%s:%s:%s:%s:%s:%d",
			event.Type,
			resourceConfig.Kind,
			metadata.GetNamespace(),
			metadata.GetName(),
			metadata.GetResourceVersion(),
			watchState.ReconnectCount) // Include reconnect count to handle reconnection scenarios

		// Check if we've already processed this event
		if _, exists := processedEvents[eventKey]; exists {
			log.Printf("Skipping duplicate event: %s", eventKey)
			w.metrics.EventsSkipped++
			continue
		}

		// Mark event as processed with TTL
		processedEvents[eventKey] = time.Now()

		// Update metrics
		w.metrics.EventsReceived++

		// Check if this is a new resource or an existing one
		key := fmt.Sprintf("%s/%s", metadata.GetNamespace(), metadata.GetName())
		_, isExisting := watchState.InitialResources[key]

		// Track if we actually processed this event
		eventProcessed := false

		// Log the event and send notification
		switch event.Type {
		case "ADDED":
			// Only send ADDED notifications for resources that are truly new
			// A resource is considered new if:
			// 1. It's not in our initial resources list AND
			// 2. It was created after we started watching (with a small buffer)
			resourceCreationTime := metadata.GetCreationTimestamp()
			timeSinceWatchStart := time.Since(watchStartTime)

			if !isExisting && watchState.IsInitialized && timeSinceWatchStart > 30*time.Second {
				// Additional check: if we have creation timestamp, verify it's recent
				if !resourceCreationTime.IsZero() {
					timeSinceCreation := time.Since(resourceCreationTime.Time)
					if timeSinceCreation < timeSinceWatchStart+30*time.Second {
						log.Printf("[%s] New resource %s/%s was ADDED by %s (created %v ago)",
							resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName(), user, timeSinceCreation)
						w.notifier.SendNotification(notifier.NotificationEvent{
							EventType:    "ADDED",
							ResourceKind: resourceConfig.Kind,
							ResourceName: metadata.GetName(),
							Namespace:    metadata.GetNamespace(),
							User:         user,
						})
						w.metrics.EventsProcessed++
						eventProcessed = true
					} else {
						log.Printf("[%s] Resource %s/%s was created before watch start (no notification sent)",
							resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName())
						w.metrics.EventsSkipped++
					}
				} else {
					// No creation timestamp, use the time-based approach
					log.Printf("[%s] New resource %s/%s was ADDED by %s (no creation timestamp)",
						resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName(), user)
					w.notifier.SendNotification(notifier.NotificationEvent{
						EventType:    "ADDED",
						ResourceKind: resourceConfig.Kind,
						ResourceName: metadata.GetName(),
						Namespace:    metadata.GetNamespace(),
						User:         user,
					})
					w.metrics.EventsProcessed++
					eventProcessed = true
				}
			} else if isExisting {
				log.Printf("[%s] Existing resource %s/%s discovered during startup (no notification sent)",
					resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName())
				w.metrics.EventsSkipped++
			} else {
				log.Printf("[%s] Resource %s/%s discovered during startup period (no notification sent)",
					resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName())
				w.metrics.EventsSkipped++
			}
			// Always update tracking to prevent future spam
			watchState.InitialResources[key] = resourceInfo{
				version:  metadata.GetResourceVersion(),
				lastSeen: time.Now(),
			}
		case "MODIFIED":
			// Only send MODIFIED notifications for actual modifications
			// Check if this is a real modification by comparing resource versions
			if existingInfo, exists := watchState.InitialResources[key]; exists {
				if existingInfo.version != metadata.GetResourceVersion() {
					log.Printf("[%s] Resource %s/%s was MODIFIED by %s (version: %s -> %s)",
						resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName(), user,
						existingInfo.version, metadata.GetResourceVersion())
					w.notifier.SendNotification(notifier.NotificationEvent{
						EventType:    "MODIFIED",
						ResourceKind: resourceConfig.Kind,
						ResourceName: metadata.GetName(),
						Namespace:    metadata.GetNamespace(),
						User:         user,
					})
					w.metrics.EventsProcessed++
					eventProcessed = true
				} else {
					log.Printf("[%s] Resource %s/%s MODIFIED event with same version (no notification sent)",
						resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName())
					w.metrics.EventsSkipped++
				}
			} else {
				// Resource not in our tracking, but we're getting a MODIFIED event
				// This could happen if we missed the ADDED event or after a reconnect
				log.Printf("[%s] Resource %s/%s MODIFIED but not in tracking (sending notification)",
					resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName())
				w.notifier.SendNotification(notifier.NotificationEvent{
					EventType:    "MODIFIED",
					ResourceKind: resourceConfig.Kind,
					ResourceName: metadata.GetName(),
					Namespace:    metadata.GetNamespace(),
					User:         user,
				})
				w.metrics.EventsProcessed++
				eventProcessed = true
			}
			// Update tracking
			watchState.InitialResources[key] = resourceInfo{
				version:  metadata.GetResourceVersion(),
				lastSeen: time.Now(),
			}
		case "DELETED":
			// Only send DELETED notifications for resources we were tracking
			if _, exists := watchState.InitialResources[key]; exists {
				log.Printf("[%s] Resource %s/%s was DELETED by %s",
					resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName(), user)
				w.notifier.SendNotification(notifier.NotificationEvent{
					EventType:    "DELETED",
					ResourceKind: resourceConfig.Kind,
					ResourceName: metadata.GetName(),
					Namespace:    metadata.GetNamespace(),
					User:         user,
				})
				w.metrics.EventsProcessed++
				eventProcessed = true
			} else {
				log.Printf("[%s] Resource %s/%s DELETED but not in tracking (no notification sent)",
					resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName())
				w.metrics.EventsSkipped++
			}
			// Remove from tracking
			delete(watchState.InitialResources, key)
		default:
			log.Printf("Skipping unknown event type: %s", event.Type)
			w.metrics.EventsSkipped++
			continue
		}

		// Call the event handler only for events that were actually processed
		if eventProcessed {
			w.eventHandler(resourceEvent)
		}
	}
}
