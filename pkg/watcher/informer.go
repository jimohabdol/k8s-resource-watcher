package watcher

import (
	"context"
	"fmt"
	"log"
	"reflect"

	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/jimohabdol/k8s-resource-watcher/pkg/config"
	"github.com/jimohabdol/k8s-resource-watcher/pkg/notifier"

	appsv1 "k8s.io/api/apps/v1"
)

// InformerWatcher represents a Kubernetes resource watcher using Informers
type InformerWatcher struct {
	config             *config.Config
	notifier           notifier.Notifier
	dynamicClient      dynamic.Interface
	k8sClient          *kubernetes.Clientset
	informerFactory    dynamicinformer.DynamicSharedInformerFactory
	k8sInformerFactory informers.SharedInformerFactory

	informers map[string]cache.SharedIndexInformer

	mu        sync.RWMutex
	ctx       context.Context
	cancel    context.CancelFunc
	isStarted bool
}

func NewInformerWatcher(cfg *config.Config, notifier notifier.Notifier) (*InformerWatcher, error) {
	// Load kubeconfig
	kubeconfig, err := clientcmd.BuildConfigFromFlags("", "")
	if err != nil {
		return nil, fmt.Errorf("failed to build kubeconfig: %w", err)
	}

	// Create dynamic client
	dynamicClient, err := dynamic.NewForConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	// Create Kubernetes client
	k8sClient, err := kubernetes.NewForConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	// Create shared informer factories
	informerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 0)
	k8sInformerFactory := informers.NewSharedInformerFactory(k8sClient, 0)

	ctx, cancel := context.WithCancel(context.Background())

	watcher := &InformerWatcher{
		config:             cfg,
		notifier:           notifier,
		dynamicClient:      dynamicClient,
		k8sClient:          k8sClient,
		informerFactory:    informerFactory,
		k8sInformerFactory: k8sInformerFactory,
		informers:          make(map[string]cache.SharedIndexInformer),
		ctx:                ctx,
		cancel:             cancel,
		isStarted:          false,
	}

	return watcher, nil
}

// Start begins watching all configured resources
func (w *InformerWatcher) Start() error {
	log.Printf("Starting Informer-based resource watcher...")

	// Create and start informers for each resource type
	for _, resourceConfig := range w.config.Resources {
		if err := w.createInformer(resourceConfig); err != nil {
			log.Printf("Failed to create informer for %s: %v", resourceConfig.Kind, err)
			continue
		}
	}

	// Start all informers
	w.informerFactory.Start(w.ctx.Done())
	w.k8sInformerFactory.Start(w.ctx.Done())

	// Wait for caches to sync
	log.Printf("Waiting for informer caches to sync...")
	if !cache.WaitForCacheSync(w.ctx.Done(), w.getCacheSyncFuncs()...) {
		return fmt.Errorf("failed to sync informer caches")
	}

	// Set the startup flag AFTER caches are synced
	w.mu.Lock()
	w.isStarted = true
	w.mu.Unlock()

	log.Printf("All informer caches synced successfully")
	return nil
}

// Stop gracefully shuts down the watcher
func (w *InformerWatcher) Stop() {
	log.Printf("Stopping Informer-based resource watcher...")
	w.cancel()
	log.Printf("Informer-based resource watcher stopped")
}

// createInformer creates an informer for a specific resource type
func (w *InformerWatcher) createInformer(resourceConfig config.ResourceConfig) error {
	var informer cache.SharedIndexInformer

	switch resourceConfig.Kind {
	case "Deployment":
		// Use Kubernetes client informer for Deployments (better type safety)
		deploymentInformer := w.k8sInformerFactory.Apps().V1().Deployments().Informer()
		deploymentInformer.AddEventHandler(w.createDeploymentEventHandler(resourceConfig))
		informer = deploymentInformer

	case "ConfigMap":
		configMapInformer := w.informerFactory.ForResource(schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "configmaps",
		}).Informer()
		configMapInformer.AddEventHandler(w.createResourceEventHandler(resourceConfig, "ConfigMap"))
		informer = configMapInformer

	case "Secret":
		secretInformer := w.informerFactory.ForResource(schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "secrets",
		}).Informer()
		secretInformer.AddEventHandler(w.createResourceEventHandler(resourceConfig, "Secret"))
		informer = secretInformer

	case "Service":
		serviceInformer := w.informerFactory.ForResource(schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "services",
		}).Informer()
		serviceInformer.AddEventHandler(w.createResourceEventHandler(resourceConfig, "Service"))
		informer = serviceInformer

	case "Ingress":
		ingressInformer := w.informerFactory.ForResource(schema.GroupVersionResource{
			Group:    "networking.k8s.io",
			Version:  "v1",
			Resource: "ingresses",
		}).Informer()
		ingressInformer.AddEventHandler(w.createResourceEventHandler(resourceConfig, "Ingress"))
		informer = ingressInformer

	default:
		return fmt.Errorf("unsupported resource kind: %s", resourceConfig.Kind)
	}

	// Store informer reference
	w.mu.Lock()
	w.informers[resourceConfig.Kind] = informer
	w.mu.Unlock()

	// Log the monitoring configuration
	if resourceConfig.ResourceName != "" {
		if resourceConfig.Namespace != "" {
			log.Printf("Created informer for %s '%s' in namespace '%s'",
				resourceConfig.Kind, resourceConfig.ResourceName, resourceConfig.Namespace)
		} else {
			log.Printf("Created informer for %s '%s' across all namespaces",
				resourceConfig.Kind, resourceConfig.ResourceName)
		}
	} else if resourceConfig.Namespace != "" {
		log.Printf("Created informer for all %s resources in namespace '%s'",
			resourceConfig.Kind, resourceConfig.Namespace)
	} else {
		log.Printf("Created informer for all %s resources across all namespaces",
			resourceConfig.Kind)
	}
	return nil
}

// createResourceEventHandler creates event handlers for infrastructure resources
func (w *InformerWatcher) createResourceEventHandler(resourceConfig config.ResourceConfig, resourceKind string) cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if !w.isStarted {
				log.Printf("[%s] Resource discovered during startup sync - skipping notification", resourceKind)
				return
			}
			w.handleResourceAdded(obj, resourceConfig, resourceKind)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if !w.isStarted {
				return
			}
			w.handleResourceUpdated(oldObj, newObj, resourceConfig, resourceKind)
		},
		DeleteFunc: func(obj interface{}) {
			// Skip notifications during startup sync
			if !w.isStarted {
				return
			}
			w.handleResourceDeleted(obj, resourceConfig, resourceKind)
		},
	}
}

// createDeploymentEventHandler creates event handlers specifically for Deployments
func (w *InformerWatcher) createDeploymentEventHandler(resourceConfig config.ResourceConfig) cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// Skip notifications during startup sync
			w.mu.RLock()
			started := w.isStarted
			w.mu.RUnlock()

			if !started {
				log.Printf("[Deployment] Resource discovered during startup sync - will track for important field changes")
				return
			}
			w.handleDeploymentAdded(obj, resourceConfig)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			// Skip notifications during startup sync
			w.mu.RLock()
			started := w.isStarted
			w.mu.RUnlock()

			if !started {
				return
			}
			w.handleDeploymentUpdated(oldObj, newObj, resourceConfig)
		},
		DeleteFunc: func(obj interface{}) {
			// Skip notifications during startup sync
			w.mu.RLock()
			started := w.isStarted
			w.mu.RUnlock()

			if !started {
				return
			}
			w.handleDeploymentDeleted(obj, resourceConfig)
		},
	}
}

// handleResourceAdded handles ADDED events for infrastructure resources
func (w *InformerWatcher) handleResourceAdded(obj interface{}, resourceConfig config.ResourceConfig, resourceKind string) {
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		log.Printf("Failed to convert %s to unstructured object", resourceKind)
		return
	}

	if !w.shouldProcessResource(unstructuredObj, resourceConfig) {
		return
	}

	log.Printf("[%s] Resource %s/%s was ADDED", resourceKind, unstructuredObj.GetNamespace(), unstructuredObj.GetName())

	// Send immediate notification for infrastructure resources
	w.sendNotification(resourceKind, "ADDED", unstructuredObj.GetName(), unstructuredObj.GetNamespace())
}

// handleResourceUpdated handles MODIFIED events for infrastructure resources
func (w *InformerWatcher) handleResourceUpdated(oldObj, newObj interface{}, resourceConfig config.ResourceConfig, resourceKind string) {
	_, ok := oldObj.(*unstructured.Unstructured)
	if !ok {
		log.Printf("Failed to convert old %s to unstructured object", resourceKind)
		return
	}

	newUnstructured, ok := newObj.(*unstructured.Unstructured)
	if !ok {
		log.Printf("Failed to convert new %s to unstructured object", resourceKind)
		return
	}

	if !w.shouldProcessResource(newUnstructured, resourceConfig) {
		return
	}

	log.Printf("[%s] Resource %s/%s was MODIFIED", resourceKind, newUnstructured.GetNamespace(), newUnstructured.GetName())

	// Send immediate notification for infrastructure resources
	w.sendNotification(resourceKind, "MODIFIED", newUnstructured.GetName(), newUnstructured.GetNamespace())
}

// handleResourceDeleted handles DELETED events for infrastructure resources
func (w *InformerWatcher) handleResourceDeleted(obj interface{}, resourceConfig config.ResourceConfig, resourceKind string) {
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		log.Printf("Failed to convert %s to unstructured object", resourceKind)
		return
	}

	if !w.shouldProcessResource(unstructuredObj, resourceConfig) {
		return
	}

	log.Printf("[%s] Resource %s/%s was DELETED", resourceKind, unstructuredObj.GetNamespace(), unstructuredObj.GetName())

	// Send immediate notification for infrastructure resources
	w.sendNotification(resourceKind, "DELETED", unstructuredObj.GetName(), unstructuredObj.GetNamespace())
}

// handleDeploymentAdded handles ADDED events for Deployments
func (w *InformerWatcher) handleDeploymentAdded(obj interface{}, resourceConfig config.ResourceConfig) {
	deployment, ok := obj.(*appsv1.Deployment)
	if !ok {
		log.Printf("[Deployment] Failed to convert to deployment object")
		return
	}

	if !w.shouldProcessDeployment(deployment, resourceConfig) {
		return
	}

	log.Printf("[Deployment] Resource %s/%s was ADDED", deployment.Namespace, deployment.Name)

	w.sendNotification("Deployment", "ADDED", deployment.Name, deployment.Namespace)
}

// handleDeploymentUpdated handles MODIFIED events for Deployments
func (w *InformerWatcher) handleDeploymentUpdated(oldObj, newObj interface{}, resourceConfig config.ResourceConfig) {
	oldDeployment, ok := oldObj.(*appsv1.Deployment)
	if !ok {
		log.Printf("Failed to convert old deployment to typed object")
		return
	}

	newDeployment, ok := newObj.(*appsv1.Deployment)
	if !ok {
		log.Printf("Failed to convert new deployment to typed object")
		return
	}

	if !w.shouldProcessDeployment(newDeployment, resourceConfig) {
		return
	}

	// Only notify if important fields have changed
	if w.hasImportantDeploymentChanges(oldDeployment, newDeployment) {
		log.Printf("[Deployment] Important fields changed for %s/%s", newDeployment.Namespace, newDeployment.Name)
		w.sendNotification("Deployment", "MODIFIED", newDeployment.Name, newDeployment.Namespace)
	} else {
		log.Printf("[Deployment] Non-important changes detected for %s/%s (skipping notification)", newDeployment.Namespace, newDeployment.Name)
	}
}

// hasImportantDeploymentChanges checks if any important fields have changed
func (w *InformerWatcher) hasImportantDeploymentChanges(oldDeployment, newDeployment *appsv1.Deployment) bool {
	if !reflect.DeepEqual(oldDeployment.Spec.Template.Spec.Containers, newDeployment.Spec.Template.Spec.Containers) {
		return true
	}

	if !reflect.DeepEqual(oldDeployment.Spec.Template.Spec.Volumes, newDeployment.Spec.Template.Spec.Volumes) {
		return true
	}

	if oldDeployment.Spec.Template.Spec.ServiceAccountName != newDeployment.Spec.Template.Spec.ServiceAccountName {
		return true
	}

	if !reflect.DeepEqual(oldDeployment.Spec.Template.Spec.NodeSelector, newDeployment.Spec.Template.Spec.NodeSelector) {
		return true
	}

	if !reflect.DeepEqual(oldDeployment.Spec.Template.Spec.Affinity, newDeployment.Spec.Template.Spec.Affinity) {
		return true
	}

	if !reflect.DeepEqual(oldDeployment.Spec.Template.Spec.Tolerations, newDeployment.Spec.Template.Spec.Tolerations) {
		return true
	}

	if !reflect.DeepEqual(oldDeployment.Spec.Template.Spec.SecurityContext, newDeployment.Spec.Template.Spec.SecurityContext) {
		return true
	}

	if !reflect.DeepEqual(oldDeployment.Spec.Template.Spec.ImagePullSecrets, newDeployment.Spec.Template.Spec.ImagePullSecrets) {
		return true
	}

	if !reflect.DeepEqual(oldDeployment.Spec.Template.Spec.HostAliases, newDeployment.Spec.Template.Spec.HostAliases) {
		return true
	}

	if !reflect.DeepEqual(oldDeployment.Spec.Template.Spec.InitContainers, newDeployment.Spec.Template.Spec.InitContainers) {
		return true
	}

	return false
}

func (w *InformerWatcher) handleDeploymentDeleted(obj interface{}, resourceConfig config.ResourceConfig) {
	deployment, ok := obj.(*appsv1.Deployment)
	if !ok {
		log.Printf("[Deployment] Failed to convert to deployment object")
		return
	}

	// Check if this deployment matches our filter criteria
	if !w.shouldProcessDeployment(deployment, resourceConfig) {
		return
	}

	log.Printf("[Deployment] Resource %s/%s was DELETED", deployment.Namespace, deployment.Name)
	w.sendNotification("Deployment", "DELETED", deployment.Name, deployment.Namespace)
}

func (w *InformerWatcher) sendNotification(resourceKind, eventType, resourceName, namespace string) {
	notificationEvent := notifier.NotificationEvent{
		EventType:    eventType,
		ResourceKind: resourceKind,
		ResourceName: resourceName,
		Namespace:    namespace,
	}

	if err := w.notifier.SendNotification(notificationEvent); err != nil {
		log.Printf("Failed to send notification for %s %s/%s: %v", resourceKind, namespace, resourceName, err)
	} else {
		log.Printf("Successfully sent notification for %s %s/%s", resourceKind, namespace, resourceName)
	}
}

func (w *InformerWatcher) getCacheSyncFuncs() []cache.InformerSynced {
	var syncFuncs []cache.InformerSynced

	w.mu.RLock()
	defer w.mu.RUnlock()

	for _, informer := range w.informers {
		syncFuncs = append(syncFuncs, informer.HasSynced)
	}

	return syncFuncs
}

// shouldProcessResource checks if a resource should be processed based on configuration
func (w *InformerWatcher) shouldProcessResource(obj *unstructured.Unstructured, resourceConfig config.ResourceConfig) bool {
	objNamespace := obj.GetNamespace()
	objName := obj.GetName()

	if resourceConfig.Namespace != "" && objNamespace != resourceConfig.Namespace {
		return false
	}

	if resourceConfig.ResourceName != "" && objName != resourceConfig.ResourceName {
		return false
	}

	return true
}

// shouldProcessDeployment checks if a deployment should be processed based on configuration
func (w *InformerWatcher) shouldProcessDeployment(deployment *appsv1.Deployment, resourceConfig config.ResourceConfig) bool {
	if resourceConfig.Namespace != "" && deployment.Namespace != resourceConfig.Namespace {
		return false
	}

	if resourceConfig.ResourceName != "" && deployment.Name != resourceConfig.ResourceName {
		return false
	}

	return true
}
