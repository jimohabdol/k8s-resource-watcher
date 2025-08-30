package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"gopkg.in/yaml.v2"

	"github.com/jimohabdol/k8s-resource-watcher/pkg/config"
	"github.com/jimohabdol/k8s-resource-watcher/pkg/notifier"
	"github.com/jimohabdol/k8s-resource-watcher/pkg/watcher"

	"github.com/gin-gonic/gin"
)

func main() {
	// Parse command line flags
	configFile := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()

	// Load configuration
	cfg, err := loadConfig(*configFile)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Load logging configuration
	if err := cfg.LoadLoggingConfig(); err != nil {
		log.Printf("Warning: Failed to load logging config: %v", err)
	}

	log.Printf("Starting Kubernetes Resource Watcher (Informer-based)")
	log.Printf("Configuration loaded from: %s", *configFile)
	log.Printf("Cluster: %s", cfg.ClusterName)
	log.Printf("Watching %d resource types", len(cfg.Resources))

	// Create email notifier
	emailNotifier := notifier.NewEmailNotifier(cfg)

	// Create Informer-based watcher
	resourceWatcher, err := watcher.NewInformerWatcher(cfg, emailNotifier)
	if err != nil {
		log.Fatalf("Failed to create resource watcher: %v", err)
	}

	// Start the watcher
	if err := resourceWatcher.Start(); err != nil {
		log.Fatalf("Failed to start resource watcher: %v", err)
	}

	log.Printf("Resource watcher started successfully")

	// Set up Gin server for health checks
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	// Health check endpoints
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "OK"})
	})

	router.GET("/readyz", func(c *gin.Context) {
		if resourceWatcher != nil {
			c.JSON(200, gin.H{"status": "OK"})
		} else {
			c.JSON(503, gin.H{"status": "Not Ready"})
		}
	})

	router.GET("/", func(c *gin.Context) {
		c.JSON(200, gin.H{"message": "Kubernetes Resource Watcher is running"})
	})

	go func() {
		log.Printf("Starting health check server on port 8080")
		if err := router.Run(":8080"); err != nil {
			log.Printf("Health check server error: %v", err)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigChan
	log.Printf("Received shutdown signal: %v", sig)

	log.Printf("Shutting down resource watcher...")
	resourceWatcher.Stop()

	log.Printf("Resource watcher shutdown complete")
}

func loadConfig(configPath string) (*config.Config, error) {
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	var cfg config.Config
	if err := yaml.Unmarshal(configData, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %v", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %v", err)
	}

	if err := cfg.LoadEmailConfig(); err != nil {
		return nil, fmt.Errorf("failed to load email config: %v", err)
	}

	if clusterName := os.Getenv("CLUSTER_NAME"); clusterName != "" {
		cfg.ClusterName = clusterName
	}

	if cfg.ClusterName == "" {
		return nil, fmt.Errorf("cluster name must be set either in config.yaml or CLUSTER_NAME environment variable")
	}

	return &cfg, nil
}
