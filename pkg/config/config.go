package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type WatcherConfig struct {
	// Deployment monitoring configuration
	DeploymentImportantFields []string      `yaml:"deploymentImportantFields,omitempty"`
	EventDeduplicationWindow  time.Duration `yaml:"eventDeduplicationWindow,omitempty"`
	ResourceVersionCheck      bool          `yaml:"resourceVersionCheck,omitempty"`
	MetricsEnabled            bool          `yaml:"metricsEnabled,omitempty"`
}

type ResourceConfig struct {
	Kind         string `yaml:"kind"`
	Namespace    string `yaml:"namespace"`
	ResourceName string `yaml:"resourceName,omitempty"`
}

type EmailConfig struct {
	SMTPHost     string   `yaml:"smtpHost"`
	SMTPPort     int      `yaml:"smtpPort"`
	UseAuth      bool     `yaml:"useAuth"`
	SMTPUsername string   `yaml:"smtpUsername,omitempty"`
	SMTPPassword string   `yaml:"smtpPassword,omitempty"`
	FromEmail    string   `yaml:"fromEmail"`
	ToEmails     []string `yaml:"toEmails"`

	// TLS Configuration
	EnableTLS   bool `yaml:"enableTLS,omitempty"`
	InsecureTLS bool `yaml:"insecureTLS,omitempty"`
	ForceSSL    bool `yaml:"forceSSL,omitempty"`
}

// LoggingConfig represents configuration for logging behavior
type LoggingConfig struct {
	Level      string `yaml:"level,omitempty"`      // Log level: debug, info, warn, error (default: info)
	Format     string `yaml:"format,omitempty"`     // Log format: text, json (default: text)
	EnableJSON bool   `yaml:"enableJSON,omitempty"` // Enable JSON logging format
}

// Config represents the application configuration
type Config struct {
	ClusterName string           `yaml:"clusterName"`
	Resources   []ResourceConfig `yaml:"resources"`
	Email       EmailConfig      `yaml:"email"`
	Watcher     WatcherConfig    `yaml:"watcher,omitempty"`
	Logging     LoggingConfig    `yaml:"logging,omitempty"`
}

func (c *Config) Validate() error {
	if c.ClusterName == "" {
		return fmt.Errorf("cluster name is required")
	}

	if len(c.Resources) == 0 {
		return fmt.Errorf("at least one resource must be configured")
	}

	for i, resource := range c.Resources {
		if err := resource.Validate(); err != nil {
			return fmt.Errorf("resource[%d]: %v", i, err)
		}
	}

	if err := c.Email.Validate(); err != nil {
		return fmt.Errorf("email configuration: %v", err)
	}

	return nil
}

func (r *ResourceConfig) Validate() error {
	if r.Kind == "" {
		return fmt.Errorf("kind is required")
	}
	// Namespace can be empty to watch all namespaces
	return nil
}

func (e *EmailConfig) Validate() error {
	if e.SMTPHost == "" {
		return fmt.Errorf("SMTP host is required")
	}
	if e.SMTPPort <= 0 || e.SMTPPort > 65535 {
		return fmt.Errorf("SMTP port must be between 1 and 65535")
	}
	if e.FromEmail == "" {
		return fmt.Errorf("from email is required")
	}
	if len(e.ToEmails) == 0 {
		return fmt.Errorf("at least one recipient email is required")
	}

	for i, email := range e.ToEmails {
		if strings.TrimSpace(email) == "" {
			return fmt.Errorf("toEmails[%d]: email address cannot be empty", i)
		}
	}

	if e.UseAuth {
		if e.SMTPUsername == "" {
			return fmt.Errorf("SMTP username is required when authentication is enabled")
		}
		if e.SMTPPassword == "" {
			return fmt.Errorf("SMTP password is required when authentication is enabled")
		}
	}

	return nil
}

func (c *Config) LoadEmailConfig() error {
	if secretUsername, err := os.ReadFile("/etc/resource-watcher/secrets/smtp-username"); err == nil {
		c.Email.SMTPUsername = strings.TrimSpace(string(secretUsername))
		c.Email.UseAuth = true
	}
	if secretPassword, err := os.ReadFile("/etc/resource-watcher/secrets/smtp-password"); err == nil {
		c.Email.SMTPPassword = strings.TrimSpace(string(secretPassword))
		c.Email.UseAuth = true
	}
	if secretFromEmail, err := os.ReadFile("/etc/resource-watcher/secrets/from-email"); err == nil {
		c.Email.FromEmail = strings.TrimSpace(string(secretFromEmail))
	}
	if secretToEmails, err := os.ReadFile("/etc/resource-watcher/secrets/to-emails"); err == nil {
		emails := strings.Split(strings.TrimSpace(string(secretToEmails)), ",")
		for i, email := range emails {
			emails[i] = strings.TrimSpace(email)
		}
		c.Email.ToEmails = emails
	}

	if username := os.Getenv("SMTP_USERNAME"); username != "" {
		c.Email.SMTPUsername = strings.TrimSpace(username)
		c.Email.UseAuth = true
	}
	if password := os.Getenv("SMTP_PASSWORD"); password != "" {
		c.Email.SMTPPassword = strings.TrimSpace(password)
		c.Email.UseAuth = true
	}
	if fromEmail := os.Getenv("FROM_EMAIL"); fromEmail != "" {
		c.Email.FromEmail = strings.TrimSpace(fromEmail)
	}
	if toEmails := os.Getenv("TO_EMAILS"); toEmails != "" {
		emails := strings.Split(strings.TrimSpace(toEmails), ",")
		for i, email := range emails {
			emails[i] = strings.TrimSpace(email)
		}
		c.Email.ToEmails = emails
	}
	if smtpHost := os.Getenv("SMTP_HOST"); smtpHost != "" {
		c.Email.SMTPHost = strings.TrimSpace(smtpHost)
	}
	if smtpPort := os.Getenv("SMTP_PORT"); smtpPort != "" {
		var port int
		if _, err := fmt.Sscanf(smtpPort, "%d", &port); err == nil {
			c.Email.SMTPPort = port
		}
	}
	if useAuth := os.Getenv("SMTP_USE_AUTH"); useAuth != "" {
		c.Email.UseAuth = useAuth == "true"
	}

	if c.Email.SMTPPort == 0 {
		if c.Email.UseAuth {
			c.Email.SMTPPort = 587
		} else {
			c.Email.SMTPPort = 25
		}
	}

	// Validate the final configuration
	return c.Email.Validate()
}

// LoadLoggingConfig loads logging configuration from environment variables
func (c *Config) LoadLoggingConfig() error {
	// Set defaults
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "text"
	}

	// Override with environment variables
	if logLevel := os.Getenv("LOG_LEVEL"); logLevel != "" {
		c.Logging.Level = strings.ToLower(strings.TrimSpace(logLevel))
	}
	if logFormat := os.Getenv("LOG_FORMAT"); logFormat != "" {
		c.Logging.Format = strings.ToLower(strings.TrimSpace(logFormat))
	}
	if enableJSON := os.Getenv("LOG_JSON"); enableJSON != "" {
		c.Logging.EnableJSON = enableJSON == "true"
	}

	// Validate log level
	validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLevels[c.Logging.Level] {
		return fmt.Errorf("invalid log level: %s (valid levels: debug, info, warn, error)", c.Logging.Level)
	}

	// Validate log format
	validFormats := map[string]bool{"text": true, "json": true}
	if !validFormats[c.Logging.Format] {
		return fmt.Errorf("invalid log format: %s (valid formats: text, json)", c.Logging.Format)
	}

	return nil
}

// GetDeploymentImportantFields returns the deployment important fields, with defaults if not configured
func (w *WatcherConfig) GetDeploymentImportantFields() []string {
	if len(w.DeploymentImportantFields) > 0 {
		return w.DeploymentImportantFields
	}

	// Default important fields for deployments - production-ready defaults
	return []string{
		"containers",         // Image, env vars, resources, ports
		"volumes",            // Volume mounts and configurations
		"serviceAccountName", // Service account changes
		"nodeSelector",       // Node selection rules
		"affinity",           // Pod affinity/anti-affinity
		"tolerations",        // Node tolerations
		"securityContext",    // Security settings
		"imagePullSecrets",   // Image pull secrets
		"hostAliases",        // Host alias configurations
		"initContainers",     // Init container changes
	}
}

// GetEventDeduplicationWindow returns the deduplication window with a sensible default
func (w *WatcherConfig) GetEventDeduplicationWindow() time.Duration {
	if w.EventDeduplicationWindow > 0 {
		return w.EventDeduplicationWindow
	}
	return 30 * time.Second // Default 30 seconds
}

// IsResourceVersionCheckEnabled returns whether resource version checking is enabled
func (w *WatcherConfig) IsResourceVersionCheckEnabled() bool {
	return w.ResourceVersionCheck
}

// IsMetricsEnabled returns whether metrics collection is enabled
func (w *WatcherConfig) IsMetricsEnabled() bool {
	return w.MetricsEnabled
}
