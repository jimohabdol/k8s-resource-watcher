package config

import (
	"fmt"
	"os"
)

// Config represents the main configuration for the application
type Config struct {
	Resources   []ResourceConfig `yaml:"resources"`
	EmailConfig EmailConfig      `yaml:"email"`
	ClusterName string           `yaml:"clusterName"`
}

// ResourceConfig defines a Kubernetes resource to watch
type ResourceConfig struct {
	Kind      string `yaml:"kind"`      // e.g., "ConfigMap", "Secret"
	Namespace string `yaml:"namespace"` // namespace to watch, empty means all namespaces
}

// EmailConfig contains email notification settings
type EmailConfig struct {
	SMTPHost     string `yaml:"smtpHost"`
	SMTPPort     int    `yaml:"smtpPort"`
	SMTPUsername string `yaml:"smtpUsername,omitempty"` // Made optional
	SMTPPassword string `yaml:"smtpPassword,omitempty"` // Made optional
	FromEmail    string `yaml:"fromEmail"`
	ToEmail      string `yaml:"toEmail"`
	UseAuth      bool   `yaml:"useAuth"` // New field to control authentication
}

// LoadEmailConfig loads email configuration with the following priority:
// 1. Environment variables (highest priority)
// 2. Kubernetes Secrets (if running in k8s and secrets are mounted)
// 3. Config file values (lowest priority)
func (c *Config) LoadEmailConfig() error {
	// First, try to load from mounted Kubernetes secrets
	if secretUsername, err := os.ReadFile("/etc/resource-watcher/secrets/smtp-username"); err == nil {
		c.EmailConfig.SMTPUsername = string(secretUsername)
		c.EmailConfig.UseAuth = true
	}
	if secretPassword, err := os.ReadFile("/etc/resource-watcher/secrets/smtp-password"); err == nil {
		c.EmailConfig.SMTPPassword = string(secretPassword)
		c.EmailConfig.UseAuth = true
	}
	if secretFromEmail, err := os.ReadFile("/etc/resource-watcher/secrets/from-email"); err == nil {
		c.EmailConfig.FromEmail = string(secretFromEmail)
	}
	if secretToEmail, err := os.ReadFile("/etc/resource-watcher/secrets/to-email"); err == nil {
		c.EmailConfig.ToEmail = string(secretToEmail)
	}

	// Then, check environment variables (highest priority)
	if username := os.Getenv("SMTP_USERNAME"); username != "" {
		c.EmailConfig.SMTPUsername = username
		c.EmailConfig.UseAuth = true
	}
	if password := os.Getenv("SMTP_PASSWORD"); password != "" {
		c.EmailConfig.SMTPPassword = password
		c.EmailConfig.UseAuth = true
	}
	if fromEmail := os.Getenv("FROM_EMAIL"); fromEmail != "" {
		c.EmailConfig.FromEmail = fromEmail
	}
	if toEmail := os.Getenv("TO_EMAIL"); toEmail != "" {
		c.EmailConfig.ToEmail = toEmail
	}
	if smtpHost := os.Getenv("SMTP_HOST"); smtpHost != "" {
		c.EmailConfig.SMTPHost = smtpHost
	}
	if smtpPort := os.Getenv("SMTP_PORT"); smtpPort != "" {
		var port int
		if _, err := fmt.Sscanf(smtpPort, "%d", &port); err == nil {
			c.EmailConfig.SMTPPort = port
		}
	}
	if useAuth := os.Getenv("SMTP_USE_AUTH"); useAuth != "" {
		c.EmailConfig.UseAuth = useAuth == "true"
	}

	// Validate required fields
	if c.EmailConfig.SMTPHost == "" {
		return fmt.Errorf("SMTP host is required")
	}
	if c.EmailConfig.FromEmail == "" {
		return fmt.Errorf("From email is required")
	}
	if c.EmailConfig.ToEmail == "" {
		return fmt.Errorf("To email is required")
	}

	// Only validate auth credentials if authentication is enabled
	if c.EmailConfig.UseAuth {
		if c.EmailConfig.SMTPUsername == "" {
			return fmt.Errorf("SMTP username is required when authentication is enabled")
		}
		if c.EmailConfig.SMTPPassword == "" {
			return fmt.Errorf("SMTP password is required when authentication is enabled")
		}
	}

	return nil
}
