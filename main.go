package main

import (
	"context"
	"flag"
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

	// Start watching resources
	if err := resourceWatcher.Start(ctx); err != nil {
		log.Fatalf("Error starting resource watcher: %v", err)
	}

	// Mark application as ready
	healthHandler.SetReady(true)
	log.Printf("Resource watcher started successfully on cluster '%s' with email notifications to %s",
		cfg.ClusterName, strings.Join(cfg.Email.ToEmails, ", "))

	<-ctx.Done()
	// Mark application as not ready during shutdown
	healthHandler.SetReady(false)
	log.Println("Shutting down...")
}
