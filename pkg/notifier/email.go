package notifier

import (
	"crypto/tls"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jimohabdol/k8s-resource-watcher/pkg/config"

	"gopkg.in/gomail.v2"
)

// EmailMetrics tracks metrics for email notifications
type EmailMetrics struct {
	EmailsSent    int64
	EmailsFailed  int64
	EmailsSkipped int64
}

// EmailNotifier sends email notifications for resource events
type EmailNotifier struct {
	config  *config.Config
	metrics *EmailMetrics
	mu      sync.RWMutex
	dialer  *gomail.Dialer
}

// NewEmailNotifier creates a new email notifier
func NewEmailNotifier(cfg *config.Config) *EmailNotifier {
	// Create dialer with appropriate settings
	username := cfg.Email.SMTPUsername
	if username == "" && cfg.Email.UseAuth {
		username = ""
	}

	dialer := gomail.NewDialer(
		cfg.Email.SMTPHost,
		cfg.Email.SMTPPort,
		username,
		cfg.Email.SMTPPassword,
	)

	switch cfg.Email.SMTPPort {
	case 465:
		dialer.SSL = true
		dialer.TLSConfig = &tls.Config{
			InsecureSkipVerify: cfg.Email.InsecureTLS,
			ServerName:         cfg.Email.SMTPHost,
		}
	case 587:
		dialer.SSL = cfg.Email.ForceSSL
		dialer.TLSConfig = &tls.Config{
			InsecureSkipVerify: cfg.Email.InsecureTLS,
			ServerName:         cfg.Email.SMTPHost,
		}
	case 25:
		dialer.SSL = cfg.Email.ForceSSL
		dialer.TLSConfig = &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         cfg.Email.SMTPHost,
		}
	default:
		dialer.SSL = cfg.Email.ForceSSL
		dialer.TLSConfig = &tls.Config{
			InsecureSkipVerify: cfg.Email.InsecureTLS,
			ServerName:         cfg.Email.SMTPHost,
		}
	}

	return &EmailNotifier{
		config:  cfg,
		metrics: &EmailMetrics{},
		dialer:  dialer,
	}
}

// SendNotification sends an email notification for a resource event
func (n *EmailNotifier) SendNotification(event NotificationEvent) error {
	// Skip non-standard events
	switch event.EventType {
	case "ADDED", "MODIFIED", "DELETED", "ROLLOUT_COMPLETED":
		// Process these events
	default:
		log.Printf("Skipping notification for event type: %s", event.EventType)
		n.metrics.EmailsSkipped++
		return nil
	}

	// Create email message
	subject := fmt.Sprintf("[%s] %s %s/%s was %s",
		n.config.ClusterName,
		event.ResourceKind,
		event.Namespace,
		event.ResourceName,
		event.EventType)

	body := fmt.Sprintf(`
Resource Change Notification

Cluster: %s
Resource: %s
Name: %s
Namespace: %s
Event: %s
Time: %s

This is an automated notification from the Kubernetes Resource Watcher.
`, n.config.ClusterName, event.ResourceKind, event.ResourceName, event.Namespace, event.EventType, time.Now().Format(time.RFC3339))

	log.Printf("Preparing email: Subject='%s', To='%s', From='%s'",
		subject, strings.Join(n.config.Email.ToEmails, ", "), n.config.Email.FromEmail)

	m := gomail.NewMessage()
	m.SetHeader("From", n.config.Email.FromEmail)

	recipients := make([]string, len(n.config.Email.ToEmails))
	for i, email := range n.config.Email.ToEmails {
		recipients[i] = strings.TrimSpace(email)
	}
	m.SetHeader("To", recipients...)
	m.SetHeader("Subject", subject)
	m.SetBody("text/plain", body)

	maxRetries := 3
	backoff := 1 * time.Second
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Send the email
		if err := n.dialer.DialAndSend(m); err != nil {
			lastErr = err
			log.Printf("Failed to send email notification (attempt %d/%d): %v", attempt, maxRetries, err)

			if attempt < maxRetries {
				time.Sleep(backoff)
				backoff *= 2
				continue
			}
			n.mu.Lock()
			n.metrics.EmailsFailed++
			n.mu.Unlock()
			return fmt.Errorf("failed to send email after %d attempts: %v", maxRetries, lastErr)
		}

		n.mu.Lock()
		n.metrics.EmailsSent++
		n.mu.Unlock()
		log.Printf("Successfully sent email notification for %s %s in namespace %s to %s",
			event.ResourceKind, event.ResourceName, event.Namespace, strings.Join(n.config.Email.ToEmails, ", "))
		return nil
	}

	return lastErr
}

// GetMetrics returns a copy of the current metrics
func (n *EmailNotifier) GetMetrics() EmailMetrics {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return *n.metrics
}
