# Kubernetes Resource Watcher

A robust Go application that watches Kubernetes resources and sends email notifications when changes occur. The application is designed to be configurable, reliable, and suitable for production use.

## Features

- Watch multiple Kubernetes resources (ConfigMaps, Secrets, etc.)
- Configurable resource types and namespaces
- Email notifications for resource changes (create, update, delete)
- Captures user information from change-cause annotations
- Works both in-cluster and out-of-cluster
- Graceful shutdown handling
- Automatic reconnection on watch failures
- Health check endpoints for monitoring
- Secure by default (non-root, read-only filesystem)

## Prerequisites

- Go 1.21 or higher (for development)
- Docker (for building container)
- Access to a Kubernetes cluster
- SMTP server access for email notifications

## Configuration

The application uses a hierarchical configuration system with the following priority (highest to lowest):

1. Environment Variables
2. Kubernetes Secrets (when running in K8s)
3. Configuration File (config.yaml)

### Configuration File

The application is configured via a YAML file (`config.yaml`):

```yaml
clusterName: "my-cluster"
resources:
  - kind: "ConfigMap"
    namespace: "default"
  - kind: "Secret"
    namespace: "kube-system"
email:
  smtpHost: "smtp.gmail.com"
  smtpPort: 587
  smtpUsername: "your-email@gmail.com"
  smtpPassword: "your-app-specific-password"
  fromEmail: "your-email@gmail.com"
  toEmail: "recipient@example.com"
```

### Environment Variables

The following environment variables can override the configuration:

- `SMTP_HOST`: SMTP server hostname
- `SMTP_PORT`: SMTP server port
- `SMTP_USERNAME`: SMTP username
- `SMTP_PASSWORD`: SMTP password
- `FROM_EMAIL`: Sender email address
- `TO_EMAIL`: Recipient email address

### Kubernetes Secrets

When running in Kubernetes, sensitive data can be stored in a Secret:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: resource-watcher-email-config
type: Opaque
stringData:
  smtp-username: "your-email@gmail.com"
  smtp-password: "your-app-specific-password"
  from-email: "your-email@gmail.com"
  to-email: "recipient@example.com"
```

## Installation

### Local Development

1. Clone the repository:
   ```bash
   git clone <repository-url>
   cd k8s-resource-watcher
   ```

2. Install dependencies:
   ```bash
   go mod download
   ```

3. Update the configuration:
   ```bash
   cp config.yaml.example config.yaml
   # Edit config.yaml with your settings
   ```

4. Run the application:
   ```bash
   go run main.go
   ```

### Docker Build

1. Build the container:
   ```bash
   docker build -t resource-watcher:1.0.0 .
   ```

2. Run locally (optional):
   ```bash
   docker run -v ~/.kube/config:/root/.kube/config:ro \
     -v $(pwd)/config.yaml:/app/config.yaml:ro \
     resource-watcher:1.0.0
   ```

### Kubernetes Deployment

1. Create the RBAC resources:
   ```bash
   kubectl apply -f k8s/rbac.yaml
   ```

2. Create the email configuration secret:
   ```bash
   # Update k8s/secret.yaml with your settings first
   kubectl apply -f k8s/secret.yaml
   ```

3. Create a ConfigMap with your configuration:
   ```bash
   # Update config.yaml with your settings first
   kubectl create configmap resource-watcher-config --from-file=config.yaml
   ```

4. Deploy the application:
   ```bash
   kubectl apply -f k8s/deployment.yaml
   ```

## Monitoring and Health Checks

The application exposes the following HTTP endpoints:

- `/healthz`: Liveness probe
- `/readyz`: Readiness probe

These endpoints are used by Kubernetes to monitor the application's health.

## Security Features

1. RBAC Configuration:
   - Uses minimal required permissions
   - Separate ServiceAccount for the application
   - Cluster-level or namespace-level roles as needed

2. Container Security:
   - Runs as non-root user (UID 1000)
   - Read-only root filesystem
   - No privilege escalation
   - Resource limits enforced

3. Sensitive Data:
   - Email credentials stored in Kubernetes Secrets
   - Configuration priority system for secure overrides
   - No hardcoded sensitive values

## Troubleshooting

1. Email Configuration Issues:
   - Check the logs for email sending errors
   - Verify secret mounting in the pod
   - Confirm environment variables are set correctly

2. Resource Watching Issues:
   - Verify RBAC permissions
   - Check pod logs for connection errors
   - Ensure correct namespace configuration

3. Common Error Messages:
   - "Error loading email configuration": Missing required email settings
   - "Failed to create k8s config": Kubernetes authentication issues
   - "Error watching resources": RBAC or connectivity issues

## Logging

The application uses structured logging with the following information:
- Resource changes (create/update/delete)
- Email notification status
- Watch connection status
- Health check status

Access logs using:
```bash
kubectl logs -f deployment/resource-watcher
```

## Resource Requirements

Minimum recommended resources:
- CPU: 100m
- Memory: 128Mi

Resource limits:
- CPU: 200m
- Memory: 256Mi

## Support

For issues and feature requests, please create an issue in the repository.
