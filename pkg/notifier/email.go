package notifier

import (
	"crypto/tls"
	"fmt"
	"log"
	"strings"
	"time"

	"k8s-resource-watcher/pkg/config"

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
}

// NewEmailNotifier creates a new email notifier
func NewEmailNotifier(cfg *config.Config) *EmailNotifier {
	return &EmailNotifier{
		config:  cfg,
		metrics: &EmailMetrics{},
	}
}

// SendNotification sends an email notification for a resource event
func (n *EmailNotifier) SendNotification(event NotificationEvent) error {
	// Log email configuration details
	log.Printf("Email Configuration: SMTP Host=%s, Port=%d, Auth=%v, From=%s, To=%s",
		n.config.Email.SMTPHost,
		n.config.Email.SMTPPort,
		n.config.Email.UseAuth,
		n.config.Email.FromEmail,
		strings.Join(n.config.Email.ToEmails, ", "))

	// Skip non-standard events
	switch event.EventType {
	case "ADDED", "MODIFIED", "DELETED":
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
User: %s
Time: %s

This is an automated notification from the Kubernetes Resource Watcher.
`, n.config.ClusterName, event.ResourceKind, event.ResourceName, event.Namespace, event.EventType, event.User, time.Now().Format(time.RFC3339))

	log.Printf("Preparing email: Subject='%s', To='%s', From='%s'",
		subject, strings.Join(n.config.Email.ToEmails, ", "), n.config.Email.FromEmail)

	m := gomail.NewMessage()
	m.SetHeader("From", n.config.Email.FromEmail)
	// Set all recipients at once
	m.SetHeader("To", strings.Join(n.config.Email.ToEmails, ", "))
	m.SetHeader("Subject", subject)
	m.SetBody("text/plain", body)

	// Create dialer with appropriate settings
	d := gomail.NewDialer(
		n.config.Email.SMTPHost,
		n.config.Email.SMTPPort,
		n.config.Email.SMTPUsername,
		n.config.Email.SMTPPassword,
	)

	// Configure TLS
	d.TLSConfig = &tls.Config{
		InsecureSkipVerify: false,
		ServerName:         n.config.Email.SMTPHost,
	}

	// Implement retry logic with exponential backoff
	maxRetries := 3
	backoff := 1 * time.Second
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Send the email
		if err := d.DialAndSend(m); err != nil {
			lastErr = err
			log.Printf("Failed to send email notification (attempt %d/%d): %v", attempt, maxRetries, err)

			if attempt < maxRetries {
				// Wait before retrying with exponential backoff
				time.Sleep(backoff)
				backoff *= 2 // Exponential backoff
				continue
			}
			n.metrics.EmailsFailed++
			return fmt.Errorf("failed to send email after %d attempts: %v", maxRetries, lastErr)
		}

		// Success
		n.metrics.EmailsSent++
		log.Printf("Successfully sent email notification for %s %s in namespace %s to %s",
			event.ResourceKind, event.ResourceName, event.Namespace, strings.Join(n.config.Email.ToEmails, ", "))
		return nil
	}

	return lastErr
}
