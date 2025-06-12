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
}

// ResourceEvent represents a Kubernetes resource event
type ResourceEvent struct {
	Type         watch.EventType
	ResourceKind string
	ResourceName string
	Namespace    string
	User         string
	Timestamp    time.Time
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

	for {
		watcher, err := w.client.Resource(gvr).
			Namespace(resourceConfig.Namespace).
			Watch(ctx, metav1.ListOptions{})

		if err != nil {
			log.Printf("Error watching %s in namespace %s: %v",
				resourceConfig.Kind, resourceConfig.Namespace, err)
			time.Sleep(5 * time.Second)
			continue
		}

		for event := range watcher.ResultChan() {
			metadata := event.Object.(metav1.Object)
			user := ""
			if annotations := metadata.GetAnnotations(); annotations != nil {
				user = annotations["kubernetes.io/change-cause"]
			}

			resourceEvent := &ResourceEvent{
				Type:         event.Type,
				ResourceKind: resourceConfig.Kind,
				ResourceName: metadata.GetName(),
				Namespace:    metadata.GetNamespace(),
				User:         user,
				Timestamp:    time.Now(),
			}

			w.eventHandler(resourceEvent)
		}

		// If we get here, the watch has ended, retry after a delay
		time.Sleep(5 * time.Second)
	}
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
	// Add more resource types as needed
	default:
		return schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: kind,
		}
	}
}
