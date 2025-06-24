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

	client, err := dynamic.NewForConfig(k8sConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %v", err)
	}

	return &ResourceWatcher{
		client:       client,
		config:       cfg,
		eventHandler: eventHandler,
		metrics:      &WatcherMetrics{},
		notifier:     notifier,
	}, nil
}

// Start begins watching the configured resources
func (w *ResourceWatcher) Start(ctx context.Context) error {
	for _, resource := range w.config.Resources {
		go w.watchResource(resource)
	}
	return nil
}

func (w *ResourceWatcher) watchResource(resourceConfig config.ResourceConfig) {
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

	// Create a backoff for retries
	backoff := wait.Backoff{
		Steps:    5,
		Duration: 1 * time.Second,
		Factor:   2.0,
		Jitter:   0.1,
	}

	// Initialize resource watch state
	watchState := &ResourceWatchState{
		InitialResources: make(map[string]resourceInfo),
		IsInitialized:    false,
	}

	// Cleanup goroutine for the initialResources map
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for range ticker.C {
			now := time.Now()
			for key, info := range watchState.InitialResources {
				// Remove entries older than 24 hours
				if now.Sub(info.lastSeen) > 24*time.Hour {
					delete(watchState.InitialResources, key)
				}
			}
		}
	}()

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
			existingResources, err := w.client.Resource(gvr).Namespace(resourceConfig.Namespace).List(
				context.Background(),
				listOptions,
			)
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
	log.Printf("Waiting 10 seconds before starting to watch to avoid spam notifications...")
	time.Sleep(10 * time.Second)

	// Track processed events to prevent duplicates
	processedEvents := make(map[string]time.Time)

	// Cleanup processed events periodically
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			now := time.Now()
			for eventKey, timestamp := range processedEvents {
				// Remove events older than 1 hour
				if now.Sub(timestamp) > time.Hour {
					delete(processedEvents, eventKey)
				}
			}
		}
	}()

	for {
		// Create a watch for the resource with resource version
		var watch watch.Interface
		var watchErr error

		watchOptions := metav1.ListOptions{}
		// Only use resource version if we haven't had any issues
		if lastResourceVersion != "" && watchState.ReconnectCount == 0 {
			watchOptions.ResourceVersion = lastResourceVersion
			log.Printf("Starting watch with resource version: %s", lastResourceVersion)
		} else {
			log.Printf("Starting watch without resource version (fresh start)")
		}
		if resourceConfig.ResourceName != "" {
			watchOptions.FieldSelector = fmt.Sprintf("metadata.name=%s", resourceConfig.ResourceName)
		}

		watch, watchErr = w.client.Resource(gvr).Namespace(resourceConfig.Namespace).Watch(
			context.Background(),
			watchOptions,
		)

		if watchErr != nil {
			log.Printf("Error watching %s in namespace %s: %v",
				resourceConfig.Kind, resourceConfig.Namespace, watchErr)
			w.metrics.WatchErrors++
			// Force reset on any watch error
			lastResourceVersion = ""
			watchState.ReconnectCount = 0
			watchState.InitialResources = make(map[string]resourceInfo)
			watchState.IsInitialized = false
			time.Sleep(backoff.Step())
			continue
		}

		// Process events
		for event := range watch.ResultChan() {
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
			eventKey := fmt.Sprintf("%s:%s:%s:%s:%s",
				event.Type,
				resourceConfig.Kind,
				metadata.GetNamespace(),
				metadata.GetName(),
				metadata.GetResourceVersion())

			// Check if we've already processed this event
			if _, exists := processedEvents[eventKey]; exists {
				log.Printf("Skipping duplicate event: %s", eventKey)
				w.metrics.EventsSkipped++
				continue
			}

			// Mark event as processed
			processedEvents[eventKey] = time.Now()

			// Update metrics
			w.metrics.EventsReceived++

			// Check if this is a new resource or an existing one
			key := fmt.Sprintf("%s/%s", metadata.GetNamespace(), metadata.GetName())
			_, isExisting := watchState.InitialResources[key]

			// Log the event and send notification
			switch event.Type {
			case "ADDED":
				if !isExisting && watchState.IsInitialized && time.Since(watchState.InitializedTime) > 30*time.Second {
					log.Printf("[%s] New resource %s/%s was ADDED by %s",
						resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName(), user)
					w.notifier.SendNotification(notifier.NotificationEvent{
						EventType:    "ADDED",
						ResourceKind: resourceConfig.Kind,
						ResourceName: metadata.GetName(),
						Namespace:    metadata.GetNamespace(),
						User:         user,
					})
					w.metrics.EventsProcessed++
				} else {
					log.Printf("[%s] Existing resource %s/%s was found (no notification sent)",
						resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName())
					w.metrics.EventsSkipped++
				}
				// Always update tracking to prevent future spam
				watchState.InitialResources[key] = resourceInfo{
					version:  metadata.GetResourceVersion(),
					lastSeen: time.Now(),
				}
			case "MODIFIED":
				log.Printf("[%s] Resource %s/%s was MODIFIED by %s",
					resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName(), user)
				w.notifier.SendNotification(notifier.NotificationEvent{
					EventType:    "MODIFIED",
					ResourceKind: resourceConfig.Kind,
					ResourceName: metadata.GetName(),
					Namespace:    metadata.GetNamespace(),
					User:         user,
				})
				w.metrics.EventsProcessed++
				// Update last seen time
				watchState.InitialResources[key] = resourceInfo{
					version:  metadata.GetResourceVersion(),
					lastSeen: time.Now(),
				}
			case "DELETED":
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
				// Remove from tracking
				delete(watchState.InitialResources, key)
			default:
				log.Printf("Skipping unknown event type: %s", event.Type)
				w.metrics.EventsSkipped++
				continue
			}

			// Call the event handler
			w.eventHandler(resourceEvent)
		}

		// If we get here, the watch has ended
		watchState.ReconnectCount++
		w.metrics.WatchReconnects++
		if resourceConfig.ResourceName != "" {
			log.Printf("Watch ended for %s '%s' in namespace %s (reconnect #%d), retrying in 5 seconds...",
				resourceConfig.Kind, resourceConfig.ResourceName, resourceConfig.Namespace, watchState.ReconnectCount)
		} else {
			log.Printf("Watch ended for all %s in namespace %s (reconnect #%d), retrying in 5 seconds...",
				resourceConfig.Kind, resourceConfig.Namespace, watchState.ReconnectCount)
		}

		// Reset resource version on any reconnect
		if watchState.ReconnectCount > 0 {
			log.Printf("Resetting resource version due to reconnect")
			lastResourceVersion = ""
			watchState.InitialResources = make(map[string]resourceInfo)
			watchState.IsInitialized = false
		}

		// If too many reconnects, wait longer
		if watchState.ReconnectCount > 5 {
			log.Printf("Too many reconnects, waiting longer before retry")
			time.Sleep(30 * time.Second)
			watchState.ReconnectCount = 0 // Reset counter after long wait
		}

		// Add exponential backoff for reconnection attempts
		backoffDuration := min(5*time.Second*time.Duration(watchState.ReconnectCount), 30*time.Second)
		time.Sleep(backoffDuration)
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
