package notifier

import (
	"fmt"
	"k8s-resource-watcher/pkg/config"
	"k8s-resource-watcher/pkg/watcher"
	"log"
	"time"

	"crypto/tls"

	"gopkg.in/gomail.v2"
)

// EmailNotifier handles sending email notifications
type EmailNotifier struct {
	config      *config.EmailConfig
	clusterName string
	metrics     *EmailMetrics
}

// EmailMetrics tracks email notification statistics
type EmailMetrics struct {
	EmailsSent    int64
	EmailsFailed  int64
	EmailsSkipped int64
	LastSentTime  time.Time
}

// NewEmailNotifier creates a new email notifier instance
func NewEmailNotifier(cfg *config.EmailConfig, clusterName string) *EmailNotifier {
	return &EmailNotifier{
		config:      cfg,
		clusterName: clusterName,
		metrics:     &EmailMetrics{},
	}
}

// SendNotification sends an email notification for a resource event
func (n *EmailNotifier) SendNotification(event *watcher.ResourceEvent) error {
	// Only process ADDED, MODIFIED, and DELETED events
	switch event.Type {
	case "ADDED", "MODIFIED", "DELETED":
		// Continue with notification
	default:
		n.metrics.EmailsSkipped++
		log.Printf("Skipping notification for non-standard event type: %s", event.Type)
		return nil
	}

	// Log email configuration details
	log.Printf("Email Configuration: SMTP Host=%s, Port=%d, Auth=%v, From=%s, To=%s",
		n.config.SMTPHost,
		n.config.SMTPPort,
		n.config.UseAuth,
		n.config.FromEmail,
		n.config.ToEmail)

	// Validate email configuration
	if n.config.SMTPHost == "" {
		return fmt.Errorf("SMTP host is not configured")
	}
	if n.config.FromEmail == "" {
		return fmt.Errorf("From email is not configured")
	}
	if n.config.ToEmail == "" {
		return fmt.Errorf("To email is not configured")
	}
	if n.config.UseAuth && (n.config.SMTPUsername == "" || n.config.SMTPPassword == "") {
		return fmt.Errorf("SMTP authentication is enabled but credentials are missing")
	}

	m := gomail.NewMessage()
	m.SetHeader("From", n.config.FromEmail)
	m.SetHeader("To", n.config.ToEmail)

	var action string
	switch event.Type {
	case "ADDED":
		action = "created"
	case "MODIFIED":
		action = "modified"
	case "DELETED":
		action = "deleted"
	}

	subject := fmt.Sprintf("[%s] Kubernetes Resource %s: %s/%s", n.clusterName, action, event.Namespace, event.ResourceName)
	m.SetHeader("Subject", subject)

	body := fmt.Sprintf(`
Resource Event Details:
----------------------
Cluster: %s
Resource Kind: %s
Resource Name: %s
Namespace: %s
Action: %s
Time: %s
User: %s
Resource Version: %s
`,
		n.clusterName,
		event.ResourceKind,
		event.ResourceName,
		event.Namespace,
		action,
		event.Timestamp.Format("2006-01-02 15:04:05"),
		event.User,
		event.ResourceVersion,
	)

	m.SetBody("text/plain", body)

	var d *gomail.Dialer
	if n.config.UseAuth {
		d = gomail.NewDialer(
			n.config.SMTPHost,
			n.config.SMTPPort,
			n.config.SMTPUsername,
			n.config.SMTPPassword,
		)
		log.Printf("Using authenticated SMTP connection")
	} else {
		d = &gomail.Dialer{
			Host: n.config.SMTPHost,
			Port: n.config.SMTPPort,
		}
		log.Printf("Using unauthenticated SMTP connection")
	}

	// Enable TLS
	d.SSL = false
	d.TLSConfig = &tls.Config{InsecureSkipVerify: true}

	log.Printf("Attempting to send email notification for %s %s in namespace %s from %s to %s",
		event.ResourceKind, event.ResourceName, event.Namespace, n.config.FromEmail, n.config.ToEmail)

	// Retry mechanism
	maxRetries := 3
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		if err := d.DialAndSend(m); err != nil {
			lastErr = err
			log.Printf("Failed to send email notification (attempt %d/%d): %v", i+1, maxRetries, err)
			if i < maxRetries-1 {
				time.Sleep(time.Second * time.Duration(i+1)) // Exponential backoff
				continue
			}
		} else {
			n.metrics.EmailsSent++
			n.metrics.LastSentTime = time.Now()
			log.Printf("Successfully sent email notification for %s %s in namespace %s",
				event.ResourceKind, event.ResourceName, event.Namespace)
			return nil
		}
	}

	n.metrics.EmailsFailed++
	return fmt.Errorf("failed to send email after %d attempts: %v", maxRetries, lastErr)
}
