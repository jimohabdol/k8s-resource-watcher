# Kubernetes Resource Watcher

A Kubernetes operator that watches for changes to specified resources and sends email notifications when changes occur. The watcher can monitor specific named resources or all resources of a particular kind in a namespace.

## Features

- Watch specific named resources or all resources of a kind in a namespace
- Email notifications for resource changes (CREATED, MODIFIED, DELETED)
- Support for multiple email recipients
- Configurable SMTP settings with support for authentication
- Cluster-aware notifications (includes cluster name in notifications)
- Detailed logging of resource changes and notification attempts
- Support for multiple resource types (ConfigMaps, Secrets, Deployments)
- Kubernetes-native deployment with RBAC support

## Prerequisites

- Kubernetes cluster (v1.16 or later)
- kubectl configured to access your cluster
- SMTP server for sending notifications

## Installation

1. Clone the repository:
```bash
git clone https://github.com/jimohabdol/k8s-resource-watcher.git
cd k8s-resource-watcher
```

2. Create the Kubernetes resources:
```bash
kubectl apply -f k8s/rbac.yaml
kubectl apply -f k8s/configmap.yaml
kubectl apply -f k8s/deployment.yaml
```

3. Create a secret for SMTP credentials:
```bash
kubectl create secret generic smtp-credentials \
  --from-literal=username='your-smtp-username' \
  --from-literal=password='your-smtp-password' \
  --from-literal=from-email='your-from-email' \
  --from-literal=to-emails='recipient1@example.com,recipient2@example.com,team@example.com'
```

## Configuration

The watcher is configured through a ConfigMap. Here's an example configuration:

```yaml
clusterName: "my-cluster"  # Name of your Kubernetes cluster

resources:
  # Watch all ConfigMaps in the dev namespace
  - kind: ConfigMap
    namespace: dev
  
  # Watch a specific ConfigMap in the dev namespace
  - kind: ConfigMap
    namespace: dev
    resourceName: dev-config
  
  # Watch all Deployments in the prod namespace
  - kind: Deployment
    namespace: prod
  
  # Watch a specific Deployment in the prod namespace
  - kind: Deployment
    namespace: prod
    resourceName: api-server

email:
  smtpHost: "smtp.gmail.com"
  smtpPort: 587
  useAuth: true
  smtpUsername: "your-email@gmail.com"
  smtpPassword: "your-app-password"
  fromEmail: "your-email@gmail.com"
  toEmails:  # List of recipient email addresses
    - "recipient1@example.com"
    - "recipient2@example.com"
    - "team@example.com"
```

### Configuration Options

- `clusterName`: Name of your Kubernetes cluster (used in email notifications)
- `resources`: List of resources to watch
  - `kind`: Type of resource (ConfigMap, Secret, Deployment)
  - `namespace`: Namespace to watch
  - `resourceName`: (Optional) Specific resource to watch. If not specified, watches all resources of the kind in the namespace
- `watcher`: (Optional) Watcher behavior configuration
  - `watchTimeoutSeconds`: How long each watch connection lasts (default: 600 seconds)
  - `maxReconnects`: Maximum reconnects before resetting state (default: 5)
  - `reconnectBackoffMs`: Base backoff time in milliseconds (default: 5000)
  - `heartbeatIntervalMs`: Heartbeat interval for keep-alive (default: 30000 ms)
  - `keepAliveEnabled`: Enable keep-alive functionality (default: true)
- `email`: SMTP configuration
  - `smtpHost`: SMTP server hostname
  - `smtpPort`: SMTP server port
  - `useAuth`: Whether to use SMTP authentication
  - `smtpUsername`: SMTP username (if authentication is enabled)
  - `smtpPassword`: SMTP password (if authentication is enabled)
  - `fromEmail`: Sender email address
  - `toEmails`: List of recipient email addresses

## Usage

### Watching All Resources

To watch all resources of a kind in a namespace, omit the `resourceName` field:

```yaml
resources:
  - kind: ConfigMap
    namespace: dev
```

### Watching Specific Resources

To watch a specific resource, include the `resourceName` field:

```yaml
resources:
  - kind: ConfigMap
    namespace: dev
    resourceName: dev-config
```

### Testing the Configuration

1. Create a test resource:
```bash
kubectl create configmap dev-config --from-literal=key=value -n dev
```

2. Modify the resource:
```bash
kubectl patch configmap dev-config --patch '{"data":{"key":"new-value"}}' -n dev
```

3. Delete the resource:
```bash
kubectl delete configmap dev-config -n dev
```

4. Check the logs:
```bash
kubectl logs -f deployment/resource-watcher
```

## Email Notifications

The watcher sends email notifications for the following events:
- Resource creation (CREATED)
- Resource modification (MODIFIED)
- Resource deletion (DELETED)

Email notifications include:
- Cluster name
- Resource kind
- Resource name
- Namespace
- Event type
- User who made the change
- Timestamp

Example email subject:
```
[my-cluster] ConfigMap dev/dev-config was MODIFIED
```

### Multiple Recipients

You can configure multiple email recipients in several ways:

1. In the config file:
```yaml
email:
  toEmails:
    - "recipient1@example.com"
    - "recipient2@example.com"
    - "team@example.com"
```

2. In Kubernetes secret:
```yaml
stringData:
  to-emails: "recipient1@example.com,recipient2@example.com,team@example.com"
```

3. Using environment variable:
```bash
export TO_EMAILS="recipient1@example.com,recipient2@example.com,team@example.com"
```

## Troubleshooting

### Common Issues

1. **No email notifications received**
   - Check SMTP configuration in ConfigMap
   - Verify SMTP credentials in Kubernetes secret
   - Check pod logs for SMTP connection errors
   - Verify email server settings (port, TLS, authentication)

2. **Resource changes not detected**
   - Verify RBAC permissions
   - Check if the resource is in the correct namespace
   - Ensure the resource kind is supported
   - Check pod logs for watch errors

3. **Watch connection issues**
   - Check pod logs for connection errors
   - Verify network connectivity to Kubernetes API
   - Check RBAC permissions

4. **Frequent reconnections**
   - The watcher now includes improved reconnection handling
   - Check if the issue is related to network connectivity
   - Consider adjusting `watchTimeoutSeconds` in the watcher configuration
   - Monitor logs for specific error patterns

### Reconnection Improvements

The watcher has been improved to handle reconnection issues more gracefully:

- **Intelligent Resource Version Management**: The watcher now maintains resource versions more intelligently to avoid missing events during reconnections
- **Exponential Backoff**: Implements exponential backoff for reconnection attempts to avoid overwhelming the API server
- **Configurable Timeouts**: Watch timeouts can be configured to match your cluster's requirements
- **Better Error Handling**: Improved detection and handling of network-related errors
- **Context-Aware Shutdown**: Proper graceful shutdown handling to prevent resource leaks
- **Keep-Alive Support**: HTTP keep-alive headers and heartbeat monitoring to maintain persistent connections
- **Connection Health Monitoring**: Automatic detection and recovery from stale connections

### Performance Tuning

You can tune the watcher performance by adjusting the configuration:

```yaml
watcher:
  watchTimeoutSeconds: 600  # Increase for more stable connections
  maxReconnects: 5          # Adjust based on your cluster stability
  reconnectBackoffMs: 5000  # Increase for less aggressive reconnection
  heartbeatIntervalMs: 30000 # Decrease for more frequent health checks
  keepAliveEnabled: true    # Enable to maintain persistent connections
```

**Keep-Alive Configuration Tips:**
- **heartbeatIntervalMs**: Set to 30-60 seconds for most environments
- **keepAliveEnabled**: Enable for environments with network instability
- **watchTimeoutSeconds**: Use longer timeouts (10-15 minutes) with keep-alive enabled

### Checking Logs

View the watcher logs:
```bash
kubectl logs -f deployment/resource-watcher
```

The logs will show:
- Watch initialization
- Resource changes detected
- Email notification attempts
- Any errors or issues

## Development

### Building

Build the Docker image:
```bash
docker build -t resource-watcher:latest .
```

### Running Locally

1. Set up your kubeconfig
2. Run the watcher:
```bash
go run cmd/main.go
```

## Contributing

1. Fork the repository
2. Create a feature branch
3. Commit your changes
4. Push to the branch
5. Create a Pull Request

## License

This project is licensed under the MIT License - see the LICENSE file for details.
