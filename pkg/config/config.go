package config

import (
	"fmt"
	"os"
	"strings"
)

// Config represents the application configuration
type Config struct {
	ClusterName string           `yaml:"clusterName"`
	Resources   []ResourceConfig `yaml:"resources"`
	Email       EmailConfig      `yaml:"email"`
	Watcher     WatcherConfig    `yaml:"watcher,omitempty"`
}

// WatcherConfig represents configuration for the watcher behavior
type WatcherConfig struct {
	WatchTimeoutSeconds int64 `yaml:"watchTimeoutSeconds,omitempty"`
	MaxReconnects       int64 `yaml:"maxReconnects,omitempty"`
	ReconnectBackoffMs  int64 `yaml:"reconnectBackoffMs,omitempty"`
	HeartbeatIntervalMs int64 `yaml:"heartbeatIntervalMs,omitempty"`
	KeepAliveEnabled    bool  `yaml:"keepAliveEnabled,omitempty"`
}

// ResourceConfig represents configuration for a single resource type
type ResourceConfig struct {
	Kind         string `yaml:"kind"`
	Namespace    string `yaml:"namespace"`
	ResourceName string `yaml:"resourceName,omitempty"`
}

// EmailConfig represents the email notification configuration
type EmailConfig struct {
	SMTPHost     string   `yaml:"smtpHost"`
	SMTPPort     int      `yaml:"smtpPort"`
	UseAuth      bool     `yaml:"useAuth"`
	SMTPUsername string   `yaml:"smtpUsername,omitempty"`
	SMTPPassword string   `yaml:"smtpPassword,omitempty"`
	FromEmail    string   `yaml:"fromEmail"`
	ToEmails     []string `yaml:"toEmails"`
}

// Validate validates the configuration
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

// Validate validates a resource configuration
func (r *ResourceConfig) Validate() error {
	if r.Kind == "" {
		return fmt.Errorf("kind is required")
	}
	if r.Namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	return nil
}

// Validate validates email configuration
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

	// Validate email addresses
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

// LoadEmailConfig loads email configuration with the following priority:
// 1. Environment variables (highest priority)
// 2. Kubernetes Secrets (if running in k8s and secrets are mounted)
// 3. Config file values (lowest priority)
func (c *Config) LoadEmailConfig() error {
	// First, try to load from mounted Kubernetes secrets
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
		// Split the comma-separated list of emails
		emails := strings.Split(strings.TrimSpace(string(secretToEmails)), ",")
		for i, email := range emails {
			emails[i] = strings.TrimSpace(email)
		}
		c.Email.ToEmails = emails
	}

	// Then, check environment variables (highest priority)
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
		// Split the comma-separated list of emails
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

	// Set defaults if not provided
	if c.Email.SMTPPort == 0 {
		if c.Email.UseAuth {
			c.Email.SMTPPort = 587 // Default TLS port
		} else {
			c.Email.SMTPPort = 25 // Default non-TLS port
		}
	}

	// Validate the final configuration
	return c.Email.Validate()
}
