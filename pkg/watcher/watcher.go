package watcher

import (
	"context"
	"fmt"
	"k8s-resource-watcher/pkg/config"
	"k8s-resource-watcher/pkg/notifier"
	"log"
	"os"
	"strings"
	"time"

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
func NewResourceWatcher(cfg *config.Config, eventHandler func(event *ResourceEvent), notifier notifier.Notifier) (*ResourceWatcher, error) {
	// Try in-cluster config first
	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig
		k8sConfig, err = clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
		if err != nil {
			return nil, fmt.Errorf("failed to get kubernetes config: %v", err)
		}
	}

	// Set reasonable timeouts and limits for the client
	k8sConfig.Timeout = 30 * time.Second
	k8sConfig.QPS = 100
	k8sConfig.Burst = 200
	k8sConfig.RateLimiter = nil // Disable rate limiting for watches

	client, err := dynamic.NewForConfig(k8sConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &ResourceWatcher{
		client:       client,
		config:       cfg,
		eventHandler: eventHandler,
		metrics:      &WatcherMetrics{},
		notifier:     notifier,
		ctx:          ctx,
		cancel:       cancel,
	}, nil
}

// Start begins watching the configured resources
func (w *ResourceWatcher) Start(ctx context.Context) error {
	// Create a context that will be cancelled when the main context is cancelled
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Start watching each resource in its own goroutine
	for _, resource := range w.config.Resources {
		go func(resourceConfig config.ResourceConfig) {
			w.watchResource(watchCtx, resourceConfig)
		}(resource)
	}

	// Wait a moment to ensure all goroutines have started
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(100 * time.Millisecond):
		// All goroutines should have started by now
	}

	return nil
}

// Stop gracefully stops all watchers
func (w *ResourceWatcher) Stop() {
	w.cancel()
}

func (w *ResourceWatcher) watchResource(ctx context.Context, resourceConfig config.ResourceConfig) {
	// Get the GVR for the resource kind
	gvr := getGroupVersionResource(resourceConfig.Kind)
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

	// Create a backoff for retries with exponential backoff
	backoff := wait.Backoff{
		Steps:    10,
		Duration: 1 * time.Second,
		Factor:   2.0,
		Jitter:   0.1,
		Cap:      5 * time.Minute,
	}

	// Initialize resource watch state
	watchState := &ResourceWatchState{
		InitialResources:    make(map[string]resourceInfo),
		IsInitialized:       false,
		LastSuccessfulWatch: time.Now(),
		LastHeartbeat:       time.Now(),
		ConnectionHealthy:   false,
	}

	// Track when we actually start watching (after initial loading)
	watchStartTime := time.Time{}

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

	// Function to load initial resources
	loadInitialResources := func() (string, error) {
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

		watchState.IsInitialized = true
		watchState.InitializedTime = time.Now()
		log.Printf("Loaded %d existing %s resources", len(watchState.InitialResources), resourceConfig.Kind)
		return lastResourceVersion, nil
	}

	// Load initial resources
	lastResourceVersion, err := loadInitialResources()
	if err != nil {
		log.Printf("Error loading initial resources: %v", err)
		// Don't return, start watching anyway without resource version
		log.Printf("Starting watch without initial resource loading")
		lastResourceVersion = ""
		watchState.IsInitialized = true // Mark as initialized so we don't send notifications for existing resources
		watchState.InitializedTime = time.Now()
	} else {
		log.Printf("Successfully loaded initial resources, starting watch with resource version: %s", lastResourceVersion)
	}

	// Add a startup delay to avoid spam notifications
	// Use a more responsive approach that checks context cancellation
	log.Printf("Waiting 10 seconds before starting to watch to avoid spam notifications...")
	startupDelay := 10 * time.Second
	startupTimer := time.NewTimer(startupDelay)
	defer startupTimer.Stop()

	select {
	case <-ctx.Done():
		log.Printf("Context cancelled during startup delay, stopping watch for %s in namespace %s", resourceConfig.Kind, resourceConfig.Namespace)
		return
	case <-startupTimer.C:
		// Continue with normal operation
	}

	// Mark the actual watch start time
	watchStartTime = time.Now()
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

	for {
		select {
		case <-ctx.Done():
			log.Printf("Context cancelled, stopping watch for %s in namespace %s", resourceConfig.Kind, resourceConfig.Namespace)
			return
		default:
		}

		// Check if keep-alive detected a stale connection
		if keepAliveEnabled && !watchState.ConnectionHealthy && watchState.WatchInterface != nil {
			log.Printf("Keep-alive: Detected stale connection, forcing reconnection")
			watchState.WatchInterface.Stop()
			watchState.WatchInterface = nil
		}

		// Create a watch for the resource with resource version
		var watch watch.Interface
		var watchErr error

		// Get timeout from config or use default
		timeoutSeconds := int64(600) // Default 10 minutes
		if w.config.Watcher.WatchTimeoutSeconds > 0 {
			timeoutSeconds = w.config.Watcher.WatchTimeoutSeconds
		}

		watchOptions := metav1.ListOptions{
			// Set a reasonable timeout for the watch
			TimeoutSeconds: &timeoutSeconds,
		}

		// Only use resource version if we haven't had too many consecutive failures
		// and if we have a valid resource version
		if lastResourceVersion != "" && watchState.ConsecutiveFailures < 3 && len(lastResourceVersion) > 0 {
			watchOptions.ResourceVersion = lastResourceVersion
			log.Printf("Starting watch with resource version: %s", lastResourceVersion)
		} else {
			log.Printf("Starting watch without resource version (fresh start)")
		}

		if resourceConfig.ResourceName != "" {
			watchOptions.FieldSelector = fmt.Sprintf("metadata.name=%s", resourceConfig.ResourceName)
		}

		// Create a context with timeout for the watch (slightly longer than watch timeout)
		watchCtx, watchCancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds+60)*time.Second)
		watch, watchErr = w.client.Resource(gvr).Namespace(resourceConfig.Namespace).Watch(
			watchCtx,
			watchOptions,
		)
		watchCancel()

		if watchErr != nil {
			log.Printf("Error watching %s in namespace %s: %v",
				resourceConfig.Kind, resourceConfig.Namespace, watchErr)
			w.metrics.WatchErrors++
			watchState.ConsecutiveFailures++
			watchState.ConnectionHealthy = false

			// Check if it's a network-related error
			if strings.Contains(watchErr.Error(), "connection refused") ||
				strings.Contains(watchErr.Error(), "timeout") ||
				strings.Contains(watchErr.Error(), "network") {
				log.Printf("Network-related error detected, will retry with backoff")
			}

			// Check if context was cancelled
			if ctx.Err() != nil {
				log.Printf("Context cancelled during watch creation, stopping watch for %s in namespace %s",
					resourceConfig.Kind, resourceConfig.Namespace)
				return
			}

			// Only reset everything if we've had multiple consecutive failures
			if watchState.ConsecutiveFailures >= 3 {
				log.Printf("Multiple consecutive failures, forcing complete reset")
				lastResourceVersion = ""
				watchState.ReconnectCount = 0
				watchState.InitialResources = make(map[string]resourceInfo)
				watchState.IsInitialized = false
				watchState.ConsecutiveFailures = 0
			}

			// Use exponential backoff
			backoffDuration := backoff.Step()
			log.Printf("Retrying in %v...", backoffDuration)
			select {
			case <-ctx.Done():
				log.Printf("Context cancelled during backoff, stopping watch for %s in namespace %s",
					resourceConfig.Kind, resourceConfig.Namespace)
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

		log.Printf("Keep-alive: Watch connection established successfully")

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

		// If we get here, the watch has ended
		watchState.ReconnectCount++
		w.metrics.WatchReconnects++
		watchState.ConnectionHealthy = false

		// Ensure the watch interface is properly closed
		if watchState.WatchInterface != nil {
			watchState.WatchInterface.Stop()
			watchState.WatchInterface = nil
		}

		// Log detailed information about the reconnect
		timeSinceLastSuccess := time.Since(watchState.LastSuccessfulWatch)
		timeSinceLastHeartbeat := time.Since(watchState.LastHeartbeat)
		if resourceConfig.ResourceName != "" {
			log.Printf("Watch ended for %s '%s' in namespace %s (reconnect #%d, last success: %v ago, last heartbeat: %v ago), retrying...",
				resourceConfig.Kind, resourceConfig.ResourceName, resourceConfig.Namespace, watchState.ReconnectCount, timeSinceLastSuccess, timeSinceLastHeartbeat)
		} else {
			log.Printf("Watch ended for all %s in namespace %s (reconnect #%d, last success: %v ago, last heartbeat: %v ago), retrying...",
				resourceConfig.Kind, resourceConfig.Namespace, watchState.ReconnectCount, timeSinceLastSuccess, timeSinceLastHeartbeat)
		}

		// Only reset resource version on reconnects if we've had too many failures
		// This helps maintain continuity and reduces missed events
		maxReconnects := int64(5) // Default
		if w.config.Watcher.MaxReconnects > 0 {
			maxReconnects = w.config.Watcher.MaxReconnects
		}

		if watchState.ReconnectCount > maxReconnects {
			log.Printf("Too many reconnects, resetting resource version and clearing state")
			lastResourceVersion = ""
			watchState.InitialResources = make(map[string]resourceInfo)
			watchState.IsInitialized = false
			watchState.ReconnectCount = 0
			watchState.ConnectionHealthy = false
			// Clear processed events to prevent issues with stale tracking
			processedEvents = make(map[string]time.Time)
			log.Printf("Cleared processed events due to excessive reconnects")
		} else if watchState.ReconnectCount > 2 {
			// For moderate reconnects, just clear processed events but keep resource version
			processedEvents = make(map[string]time.Time)
			log.Printf("Cleared processed events due to reconnect")
		}

		// If too many reconnects, wait longer
		if watchState.ReconnectCount > maxReconnects {
			log.Printf("Too many reconnects, waiting longer before retry")
			select {
			case <-ctx.Done():
				log.Printf("Context cancelled during long reconnect wait, stopping watch for %s in namespace %s",
					resourceConfig.Kind, resourceConfig.Namespace)
				return
			case <-time.After(30 * time.Second):
			}
			watchState.ReconnectCount = 0 // Reset counter after long wait
		}

		// Add exponential backoff for reconnection attempts
		baseBackoff := 5 * time.Second
		if w.config.Watcher.ReconnectBackoffMs > 0 {
			baseBackoff = time.Duration(w.config.Watcher.ReconnectBackoffMs) * time.Millisecond
		}
		backoffDuration := min(baseBackoff*time.Duration(watchState.ReconnectCount), 30*time.Second)
		log.Printf("Waiting %v before next reconnect attempt...", backoffDuration)
		select {
		case <-ctx.Done():
			log.Printf("Context cancelled during reconnection backoff, stopping watch for %s in namespace %s",
				resourceConfig.Kind, resourceConfig.Namespace)
			return
		case <-time.After(backoffDuration):
		}
	}
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func getGroupVersionResource(kind string) schema.GroupVersionResource {
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
