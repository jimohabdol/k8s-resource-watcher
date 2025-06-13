package config

import (
	"fmt"
	"os"
)

// Config represents the application configuration
type Config struct {
	ClusterName string           `yaml:"clusterName"`
	Resources   []ResourceConfig `yaml:"resources"`
	Email       EmailConfig      `yaml:"email"`
}

// ResourceConfig represents configuration for a single resource type
type ResourceConfig struct {
	Kind         string `yaml:"kind"`
	Namespace    string `yaml:"namespace"`
	ResourceName string `yaml:"resourceName,omitempty"`
}

// EmailConfig represents the email notification configuration
type EmailConfig struct {
	SMTPHost     string `yaml:"smtpHost"`
	SMTPPort     int    `yaml:"smtpPort"`
	UseAuth      bool   `yaml:"useAuth"`
	SMTPUsername string `yaml:"smtpUsername,omitempty"`
	SMTPPassword string `yaml:"smtpPassword,omitempty"`
	FromEmail    string `yaml:"fromEmail"`
	ToEmail      string `yaml:"toEmail"`
}

// LoadEmailConfig loads email configuration with the following priority:
// 1. Environment variables (highest priority)
// 2. Kubernetes Secrets (if running in k8s and secrets are mounted)
// 3. Config file values (lowest priority)
func (c *Config) LoadEmailConfig() error {
	// First, try to load from mounted Kubernetes secrets
	if secretUsername, err := os.ReadFile("/etc/resource-watcher/secrets/smtp-username"); err == nil {
		c.Email.SMTPUsername = string(secretUsername)
		c.Email.UseAuth = true
	}
	if secretPassword, err := os.ReadFile("/etc/resource-watcher/secrets/smtp-password"); err == nil {
		c.Email.SMTPPassword = string(secretPassword)
		c.Email.UseAuth = true
	}
	if secretFromEmail, err := os.ReadFile("/etc/resource-watcher/secrets/from-email"); err == nil {
		c.Email.FromEmail = string(secretFromEmail)
	}
	if secretToEmail, err := os.ReadFile("/etc/resource-watcher/secrets/to-email"); err == nil {
		c.Email.ToEmail = string(secretToEmail)
	}

	// Then, check environment variables (highest priority)
	if username := os.Getenv("SMTP_USERNAME"); username != "" {
		c.Email.SMTPUsername = username
		c.Email.UseAuth = true
	}
	if password := os.Getenv("SMTP_PASSWORD"); password != "" {
		c.Email.SMTPPassword = password
		c.Email.UseAuth = true
	}
	if fromEmail := os.Getenv("FROM_EMAIL"); fromEmail != "" {
		c.Email.FromEmail = fromEmail
	}
	if toEmail := os.Getenv("TO_EMAIL"); toEmail != "" {
		c.Email.ToEmail = toEmail
	}
	if smtpHost := os.Getenv("SMTP_HOST"); smtpHost != "" {
		c.Email.SMTPHost = smtpHost
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

	// Validate required fields
	if c.Email.SMTPHost == "" {
		return fmt.Errorf("SMTP host is required")
	}
	if c.Email.FromEmail == "" {
		return fmt.Errorf("From email is required")
	}
	if c.Email.ToEmail == "" {
		return fmt.Errorf("To email is required")
	}

	// Only validate auth credentials if authentication is enabled
	if c.Email.UseAuth {
		if c.Email.SMTPUsername == "" {
			return fmt.Errorf("SMTP username is required when authentication is enabled")
		}
		if c.Email.SMTPPassword == "" {
			return fmt.Errorf("SMTP password is required when authentication is enabled")
		}
	}

	return nil
}
