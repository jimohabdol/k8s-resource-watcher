package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s-resource-watcher/pkg/config"
	"k8s-resource-watcher/pkg/health"
	"k8s-resource-watcher/pkg/notifier"
	"k8s-resource-watcher/pkg/watcher"

	"gopkg.in/yaml.v2"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	port := flag.String("port", "8080", "Port for health check endpoints")
	flag.Parse()

	// Read configuration
	configData, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("Error reading config file: %v", err)
	}

	var cfg config.Config
	if err := yaml.Unmarshal(configData, &cfg); err != nil {
		log.Fatalf("Error parsing config file: %v", err)
	}

	// Validate configuration
	if err := validateConfiguration(&cfg); err != nil {
		log.Fatalf("Configuration validation failed: %v", err)
	}

	// Load email configuration with priority handling
	if err := cfg.LoadEmailConfig(); err != nil {
		log.Fatalf("Error loading email configuration: %v", err)
	}

	// Check for cluster name in environment
	if clusterName := os.Getenv("CLUSTER_NAME"); clusterName != "" {
		cfg.ClusterName = clusterName
	}

	if cfg.ClusterName == "" {
		log.Fatalf("Cluster name must be set either in config.yaml or CLUSTER_NAME environment variable")
	}

	// Set up health check handler
	healthHandler := health.NewHandler()
	http.HandleFunc("/healthz", healthHandler.LivenessHandler)
	http.HandleFunc("/readyz", healthHandler.ReadinessHandler)

	// Create server with timeouts
	server := &http.Server{
		Addr:         ":" + *port,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Start health check server
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Health check server error: %v", err)
		}
	}()

	// Create email notifier
	emailNotifier := notifier.NewEmailNotifier(&cfg)

	// Create resource watcher
	resourceWatcher, err := watcher.NewResourceWatcher(&cfg, func(event *watcher.ResourceEvent) {
		if err := emailNotifier.SendNotification(notifier.NotificationEvent{
			EventType:    string(event.Type),
			ResourceKind: event.ResourceKind,
			ResourceName: event.ResourceName,
			Namespace:    event.Namespace,
			User:         event.User,
		}); err != nil {
			log.Printf("Error sending notification: %v", err)
		}
	}, emailNotifier)
	if err != nil {
		log.Fatalf("Error creating resource watcher: %v", err)
	}

	// Set up context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown gracefully
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signalChan
		log.Println("Received shutdown signal, stopping watchers...")
		cancel()
		resourceWatcher.Stop()

		// Create shutdown context with timeout
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()

		// Shutdown HTTP server
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Error shutting down HTTP server: %v", err)
		}
	}()

	// Start watching resources with startup timeout
	startCtx, startCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer startCancel()

	if err := resourceWatcher.Start(startCtx); err != nil {
		log.Fatalf("Error starting resource watcher: %v", err)
	}

	// Mark application as ready
	healthHandler.SetReady(true)
	log.Printf("Resource watcher started successfully on cluster '%s' with email notifications to %s",
		cfg.ClusterName, strings.Join(cfg.Email.ToEmails, ", "))

	// Wait for context cancellation
	<-ctx.Done()
	// Mark application as not ready during shutdown
	healthHandler.SetReady(false)
	log.Println("Shutting down...")
}

// validateConfiguration validates the configuration before starting the application
func validateConfiguration(cfg *config.Config) error {
	if len(cfg.Resources) == 0 {
		return fmt.Errorf("no resources configured to watch")
	}

	for i, resource := range cfg.Resources {
		if resource.Kind == "" {
			return fmt.Errorf("resource[%d]: kind is required", i)
		}
		if resource.Namespace == "" {
			return fmt.Errorf("resource[%d]: namespace is required", i)
		}

		// Validate resource kind
		gvr := watcher.GetGroupVersionResource(resource.Kind)
		if gvr.Empty() {
			return fmt.Errorf("resource[%d]: unknown resource kind '%s'", i, resource.Kind)
		}
	}

	// Set default watcher configuration if not provided
	if cfg.Watcher.WatchTimeoutSeconds == 0 {
		cfg.Watcher.WatchTimeoutSeconds = 600 // 10 minutes
	}
	if cfg.Watcher.MaxReconnects == 0 {
		cfg.Watcher.MaxReconnects = 5
	}
	if cfg.Watcher.ReconnectBackoffMs == 0 {
		cfg.Watcher.ReconnectBackoffMs = 5000 // 5 seconds
	}
	if cfg.Watcher.HeartbeatIntervalMs == 0 {
		cfg.Watcher.HeartbeatIntervalMs = 30000 // 30 seconds
	}

	return nil
}
