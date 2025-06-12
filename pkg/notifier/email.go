package notifier

import (
	"fmt"
	"k8s-resource-watcher/pkg/config"
	"k8s-resource-watcher/pkg/watcher"

	"log"

	"gopkg.in/gomail.v2"
)

// EmailNotifier handles sending email notifications
type EmailNotifier struct {
	config      *config.EmailConfig
	clusterName string
}

// NewEmailNotifier creates a new email notifier instance
func NewEmailNotifier(cfg *config.EmailConfig, clusterName string) *EmailNotifier {
	return &EmailNotifier{
		config:      cfg,
		clusterName: clusterName,
	}
}

// SendNotification sends an email notification for a resource event
func (n *EmailNotifier) SendNotification(event *watcher.ResourceEvent) error {
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
	default:
		action = string(event.Type)
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
`,
		n.clusterName,
		event.ResourceKind,
		event.ResourceName,
		event.Namespace,
		action,
		event.Timestamp.Format("2006-01-02 15:04:05"),
		event.User,
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
	} else {
		d = &gomail.Dialer{
			Host: n.config.SMTPHost,
			Port: n.config.SMTPPort,
		}
	}

	log.Printf("Attempting to send email notification for %s %s in namespace %s",
		event.ResourceKind, event.ResourceName, event.Namespace)

	if err := d.DialAndSend(m); err != nil {
		log.Printf("Failed to send email notification: %v", err)
		return err
	}

	log.Printf("Successfully sent email notification for %s %s in namespace %s",
		event.ResourceKind, event.ResourceName, event.Namespace)
	return nil
}
