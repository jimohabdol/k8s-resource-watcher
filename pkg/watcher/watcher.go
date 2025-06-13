package watcher

import (
	"context"
	"fmt"
	"k8s-resource-watcher/pkg/config"
	"log"
	"os"
	"time"

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
	notifier     *notifier.Notifier
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
func NewResourceWatcher(cfg *config.Config, eventHandler func(event *ResourceEvent), notifier *notifier.Notifier) (*ResourceWatcher, error) {
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

	for {
		// Create a watch for the resource
		var watch watch.Interface
		var err error

		if resourceConfig.ResourceName != "" {
			// Watch specific resource
			watch, err = w.client.Resource(gvr).Namespace(resourceConfig.Namespace).Watch(
				context.Background(),
				metav1.ListOptions{
					FieldSelector: fmt.Sprintf("metadata.name=%s", resourceConfig.ResourceName),
				},
			)
		} else {
			// Watch all resources in namespace
			watch, err = w.client.Resource(gvr).Namespace(resourceConfig.Namespace).Watch(
				context.Background(),
				metav1.ListOptions{},
			)
		}

		if err != nil {
			log.Printf("Error watching %s in namespace %s: %v",
				resourceConfig.Kind, resourceConfig.Namespace, err)
			time.Sleep(backoff.Step())
			continue
		}

		// Process events
		for event := range watch.ResultChan() {
			obj, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				log.Printf("Error: unexpected object type: %T", event.Object)
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

			// Get the user who made the change
			user := "unknown"
			if annotations := metadata.GetAnnotations(); annotations != nil {
				if u, ok := annotations["kubernetes.io/change-cause"]; ok {
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

			// Log the event
			switch event.Type {
			case watch.Added:
				log.Printf("[%s] Resource %s/%s was CREATED by %s",
					resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName(), user)
				w.notifier.SendNotification(notifier.NotificationEvent{
					EventType:    "CREATED",
					ResourceKind: resourceConfig.Kind,
					ResourceName: metadata.GetName(),
					Namespace:    metadata.GetNamespace(),
					User:         user,
				})
			case watch.Modified:
				log.Printf("[%s] Resource %s/%s was MODIFIED by %s",
					resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName(), user)
				w.notifier.SendNotification(notifier.NotificationEvent{
					EventType:    "MODIFIED",
					ResourceKind: resourceConfig.Kind,
					ResourceName: metadata.GetName(),
					Namespace:    metadata.GetNamespace(),
					User:         user,
				})
			case watch.Deleted:
				log.Printf("[%s] Resource %s/%s was DELETED by %s",
					resourceConfig.Kind, metadata.GetNamespace(), metadata.GetName(), user)
				w.notifier.SendNotification(notifier.NotificationEvent{
					EventType:    "DELETED",
					ResourceKind: resourceConfig.Kind,
					ResourceName: metadata.GetName(),
					Namespace:    metadata.GetNamespace(),
					User:         user,
				})
			}

			// Call the event handler
			w.eventHandler(resourceEvent)
		}

		// If we get here, the watch has ended
		if resourceConfig.ResourceName != "" {
			log.Printf("Watch ended for %s '%s' in namespace %s, retrying in 5 seconds...",
				resourceConfig.Kind, resourceConfig.ResourceName, resourceConfig.Namespace)
		} else {
			log.Printf("Watch ended for all %s in namespace %s, retrying in 5 seconds...",
				resourceConfig.Kind, resourceConfig.Namespace)
		}
		time.Sleep(5 * time.Second)
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
	// Add more resource types as needed
	default:
		return schema.GroupVersionResource{}
	}
}
