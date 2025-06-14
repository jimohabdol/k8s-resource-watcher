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
	EventsReceived      int64
	EventsProcessed     int64
	EventsSkipped       int64
	WatchErrors         int64
	WatchReconnects     int64
	LastResourceVersion string
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

	// Track initial resource versions with TTL
	type resourceInfo struct {
		version  string
		lastSeen time.Time
	}
	initialResources := make(map[string]resourceInfo)
	initialResourcesLoaded := false

	// Cleanup goroutine for the initialResources map
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for range ticker.C {
			now := time.Now()
			for key, info := range initialResources {
				// Remove entries older than 24 hours
				if now.Sub(info.lastSeen) > 24*time.Hour {
					delete(initialResources, key)
				}
			}
		}
	}()

	// First, get the list of existing resources with pagination
	var listOptions metav1.ListOptions
	if resourceConfig.ResourceName != "" {
		listOptions = metav1.ListOptions{
			FieldSelector: fmt.Sprintf("metadata.name=%s", resourceConfig.ResourceName),
			Limit:         500, // Use pagination
		}
	} else {
		listOptions = metav1.ListOptions{
			Limit: 500, // Use pagination
		}
	}

	// Track the last resource version for reconnection
	var lastResourceVersion string

	// List existing resources with pagination
	for {
		existingResources, err := w.client.Resource(gvr).Namespace(resourceConfig.Namespace).List(
			context.Background(),
			listOptions,
		)
		if err != nil {
			log.Printf("Error listing existing %s in namespace %s: %v",
				resourceConfig.Kind, resourceConfig.Namespace, err)
			break
		}

		// Store resource versions of existing resources
		for _, item := range existingResources.Items {
			metadata, err := meta.Accessor(&item)
			if err != nil {
				continue
			}
			key := fmt.Sprintf("%s/%s", metadata.GetNamespace(), metadata.GetName())
			initialResources[key] = resourceInfo{
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

	// Mark initial resources as loaded
	initialResourcesLoaded = true

	for {
		// Create a watch for the resource with resource version
		var watch watch.Interface
		var err error

		watchOptions := metav1.ListOptions{
			ResourceVersion: lastResourceVersion,
		}
		if resourceConfig.ResourceName != "" {
			watchOptions.FieldSelector = fmt.Sprintf("metadata.name=%s", resourceConfig.ResourceName)
		}

		watch, err = w.client.Resource(gvr).Namespace(resourceConfig.Namespace).Watch(
			context.Background(),
			watchOptions,
		)

		if err != nil {
			log.Printf("Error watching %s in namespace %s: %v",
				resourceConfig.Kind, resourceConfig.Namespace, err)
			w.metrics.WatchErrors++
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

			// Update metrics
			w.metrics.EventsReceived++
			w.metrics.LastResourceVersion = metadata.GetResourceVersion()

			// Check if this is a new resource or an existing one
			key := fmt.Sprintf("%s/%s", metadata.GetNamespace(), metadata.GetName())
			_, isExisting := initialResources[key]

			// Log the event and send notification
			switch event.Type {
			case "ADDED":
				if !isExisting && initialResourcesLoaded {
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
				// Update last seen time
				initialResources[key] = resourceInfo{
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
				initialResources[key] = resourceInfo{
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
				delete(initialResources, key)
			default:
				log.Printf("Skipping unknown event type: %s", event.Type)
				w.metrics.EventsSkipped++
				continue
			}

			// Call the event handler
			w.eventHandler(resourceEvent)
		}

		// If we get here, the watch has ended
		w.metrics.WatchReconnects++
		if resourceConfig.ResourceName != "" {
			log.Printf("Watch ended for %s '%s' in namespace %s (reconnect #%d), retrying in 5 seconds...",
				resourceConfig.Kind, resourceConfig.ResourceName, resourceConfig.Namespace, w.metrics.WatchReconnects)
		} else {
			log.Printf("Watch ended for all %s in namespace %s (reconnect #%d), retrying in 5 seconds...",
				resourceConfig.Kind, resourceConfig.Namespace, w.metrics.WatchReconnects)
		}

		if w.metrics.WatchReconnects > 2 {
			log.Printf("Too many reconnects, resetting resource version and reloading initial state")
			lastResourceVersion = ""
			w.metrics.WatchReconnects = 0

			// Reload initial resources
			initialResources = make(map[string]resourceInfo)
			initialResourcesLoaded = false

			// List existing resources again
			listOptions := metav1.ListOptions{
				Limit: 500,
			}
			if resourceConfig.ResourceName != "" {
				listOptions.FieldSelector = fmt.Sprintf("metadata.name=%s", resourceConfig.ResourceName)
			}

			for {
				existingResources, err := w.client.Resource(gvr).Namespace(resourceConfig.Namespace).List(
					context.Background(),
					listOptions,
				)
				if err != nil {
					log.Printf("Error listing existing %s in namespace %s: %v",
						resourceConfig.Kind, resourceConfig.Namespace, err)
					break
				}

				// Store resource versions of existing resources
				for _, item := range existingResources.Items {
					metadata, err := meta.Accessor(&item)
					if err != nil {
						continue
					}
					key := fmt.Sprintf("%s/%s", metadata.GetNamespace(), metadata.GetName())
					initialResources[key] = resourceInfo{
						version:  metadata.GetResourceVersion(),
						lastSeen: time.Now(),
					}
					lastResourceVersion = metadata.GetResourceVersion()
				}

				if existingResources.GetContinue() == "" {
					break
				}
				listOptions.Continue = existingResources.GetContinue()
			}
			initialResourcesLoaded = true
		}

		// Add exponential backoff for reconnection attempts
		backoffDuration := min(5*time.Second*time.Duration(w.metrics.WatchReconnects), 30*time.Second)
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
