package watcher

import (
	"context"
	"fmt"
	"k8s-resource-watcher/pkg/config"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
func NewResourceWatcher(cfg *config.Config, eventHandler func(event *ResourceEvent)) (*ResourceWatcher, error) {
	// Try in-cluster config first
	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig
		k8sConfig, err = clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
		if err != nil {
			return nil, fmt.Errorf("failed to create k8s config: %v", err)
		}
	}

	client, err := dynamic.NewForConfig(k8sConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s client: %v", err)
	}

	return &ResourceWatcher{
		client:       client,
		config:       cfg,
		eventHandler: eventHandler,
		metrics:      &WatcherMetrics{},
	}, nil
}

// Start begins watching the configured resources
func (w *ResourceWatcher) Start(ctx context.Context) error {
	for _, resource := range w.config.Resources {
		go w.watchResource(ctx, resource)
	}
	return nil
}

func (w *ResourceWatcher) watchResource(ctx context.Context, resourceConfig config.ResourceConfig) {
	gvr := getGroupVersionResource(resourceConfig.Kind)
	log.Printf("Starting to watch %s in namespace %s", resourceConfig.Kind, resourceConfig.Namespace)

	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			log.Printf("Watch context cancelled for %s in namespace %s", resourceConfig.Kind, resourceConfig.Namespace)
			return
		default:
			watcher, err := w.client.Resource(gvr).
				Namespace(resourceConfig.Namespace).
				Watch(ctx, metav1.ListOptions{
					ResourceVersion: w.metrics.LastResourceVersion,
				})

			if err != nil {
				w.metrics.WatchErrors++
				log.Printf("Error watching %s in namespace %s: %v",
					resourceConfig.Kind, resourceConfig.Namespace, err)
				time.Sleep(backoff)
				backoff = min(backoff*2, maxBackoff)
				continue
			}

			// Reset backoff on successful watch
			backoff = time.Second
			w.metrics.WatchReconnects++
			log.Printf("Watch established for %s in namespace %s", resourceConfig.Kind, resourceConfig.Namespace)

			for event := range watcher.ResultChan() {
				w.metrics.EventsReceived++

				// Skip if the event object is nil
				if event.Object == nil {
					w.metrics.EventsSkipped++
					log.Printf("Received nil event object for %s in namespace %s",
						resourceConfig.Kind, resourceConfig.Namespace)
					continue
				}

				metadata, ok := event.Object.(metav1.Object)
				if !ok {
					w.metrics.EventsSkipped++
					log.Printf("Failed to convert event object to metav1.Object for %s in namespace %s",
						resourceConfig.Kind, resourceConfig.Namespace)
					continue
				}

				// Skip if the resource name is empty
				if metadata.GetName() == "" {
					w.metrics.EventsSkipped++
					log.Printf("Received event with empty resource name for %s in namespace %s",
						resourceConfig.Kind, resourceConfig.Namespace)
					continue
				}

				// Skip if the namespace doesn't match
				if metadata.GetNamespace() != resourceConfig.Namespace {
					w.metrics.EventsSkipped++
					log.Printf("Skipping event for %s in namespace %s (watching namespace %s)",
						metadata.GetName(), metadata.GetNamespace(), resourceConfig.Namespace)
					continue
				}

				user := ""
				if annotations := metadata.GetAnnotations(); annotations != nil {
					user = annotations["kubernetes.io/change-cause"]
				}

				resourceEvent := &ResourceEvent{
					Type:            event.Type,
					ResourceKind:    resourceConfig.Kind,
					ResourceName:    metadata.GetName(),
					Namespace:       metadata.GetNamespace(),
					User:            user,
					Timestamp:       time.Now(),
					ResourceVersion: metadata.GetResourceVersion(),
				}

				// Log the event details
				switch event.Type {
				case "ADDED":
					log.Printf("[%s] Resource %s/%s was CREATED by %s",
						resourceConfig.Kind,
						metadata.GetNamespace(),
						metadata.GetName(),
						user)
				case "MODIFIED":
					log.Printf("[%s] Resource %s/%s was MODIFIED by %s",
						resourceConfig.Kind,
						metadata.GetNamespace(),
						metadata.GetName(),
						user)
				case "DELETED":
					log.Printf("[%s] Resource %s/%s was DELETED by %s",
						resourceConfig.Kind,
						metadata.GetNamespace(),
						metadata.GetName(),
						user)
				default:
					w.metrics.EventsSkipped++
					log.Printf("[%s] Resource %s/%s had event %s by %s",
						resourceConfig.Kind,
						metadata.GetNamespace(),
						metadata.GetName(),
						event.Type,
						user)
					continue // Skip non-standard events
				}

				w.metrics.EventsProcessed++
				w.metrics.LastResourceVersion = metadata.GetResourceVersion()
				w.eventHandler(resourceEvent)
			}

			// If we get here, the watch has ended, retry after a delay
			log.Printf("Watch ended for %s in namespace %s, retrying in %v...",
				resourceConfig.Kind, resourceConfig.Namespace, backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, maxBackoff)
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
	// Add more resource types as needed
	default:
		return schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: kind,
		}
	}
}
