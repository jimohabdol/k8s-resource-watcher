package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

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

	// Start health check server
	go func() {
		if err := http.ListenAndServe(":"+*port, nil); err != nil {
			log.Printf("Health check server error: %v", err)
		}
	}()

	// Create email notifier
	emailNotifier := notifier.NewEmailNotifier(&cfg.EmailConfig, cfg.ClusterName)

	// Create resource watcher
	resourceWatcher, err := watcher.NewResourceWatcher(&cfg, func(event *watcher.ResourceEvent) {
		if err := emailNotifier.SendNotification(event); err != nil {
			log.Printf("Error sending notification: %v", err)
		}
	})
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
	}()

	// Start watching resources
	if err := resourceWatcher.Start(ctx); err != nil {
		log.Fatalf("Error starting resource watcher: %v", err)
	}

	// Mark application as ready
	healthHandler.SetReady(true)
	log.Printf("Resource watcher started successfully on cluster '%s' with email notifications to %s",
		cfg.ClusterName, cfg.EmailConfig.ToEmail)

	<-ctx.Done()
	// Mark application as not ready during shutdown
	healthHandler.SetReady(false)
	log.Println("Shutting down...")
}
