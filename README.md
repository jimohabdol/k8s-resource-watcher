# Kubernetes Resource Watcher

A Kubernetes resource watcher that monitors your cluster resources and sends notifications when important changes occur.

## **Requirements**

- **Go 1.21+**
- **Kubernetes 1.20+**
- **Access to Kubernetes cluster** (via kubeconfig or in-cluster)

## **Quick Start**

### **1. Build the Application**

```bash
git clone https://github.com/jimohabdol/k8s-resource-watcher.git
cd k8s-resource-watcher

make build
```

### **2. Configure the Watcher**

Copy the example configuration and customize it:

```bash
cp config.yaml.example config.yaml
```

Edit `config.yaml` with your settings:

```yaml
clusterName: "production-cluster"

watcher:
  # Deployment monitoring - only notify when these important fields change
  deploymentImportantFields:
    - "containers"              # Image, env vars, resources, ports
    - "volumes"                 # Volume mounts and configurations
    - "serviceAccountName"      # Service account changes
    - "nodeSelector"            # Node selection rules
    - "affinity"                # Pod affinity/anti-affinity
    - "tolerations"             # Node tolerations
    - "securityContext"         # Security settings
    - "imagePullSecrets"        # Image pull secrets
    - "hostAliases"             # Host alias configurations
    - "initContainers"          # Init container changes
  
  # Enhanced features
  eventDeduplicationWindow: "30s"    # Prevent duplicate notifications
  resourceVersionCheck: true         # Enable resource version optimization
  metricsEnabled: true               # Enable metrics collection

# Email notification settings
email:
  smtpHost: "mail.example.com"
  smtpPort: 587
  useAuth: true
  smtpUsername: "your-email@example.com"
  smtpPassword: "your-password"
  fromEmail: "k8s-watcher@example.com"
  toEmails:
    - "admin@example.com"
    - "ops@example.com"

# Resources to watch
resources:
  - kind: "Deployment"
    namespace: "prod"
  - kind: "ConfigMap"
    namespace: "prod"
  - kind: "Secret"
    namespace: "prod"
  - kind: "Service"
    namespace: "prod"
  - kind: "Ingress"
    namespace: "prod"

# Logging configuration
logging:
  level: "info"
  format: "text"
  enableJSON: false
```

### **3. Run the Watcher**

```bash
make dev

./bin/resource-watcher-informer -config config.yaml
```
## **Project Structure**

```
k8s-resource-watcher/
├── 📁 pkg/                          # Core packages
│   ├── config/                      # Configuration management with smart defaults
│   ├── notifier/                    # Email notification system
│   └── watcher/                     # Resource watching logic
│       ├── informer.go              # Informer-based implementation
│       └── metrics.go               # Metrics and observability
├── 📁 k8s/                          # Kubernetes manifests
├── 📄 main.go                       # Main application with Gin health checks
├── 📄 config.yaml                   # Configuration file
├── 📄 config.yaml.example           # Configuration template
├── 📄 Makefile                      # Build and test commands
└── 📄 Dockerfile                    # Container image
```

## **Development**

### **Available Commands**

```bash
make build

make test

make dev

make clean

make docker

make help
```

### **Running Tests**

```bash
make test

go test ./pkg/... -v
```

## **Docker Deployment**

### **Build Image**

```bash
make docker
```

### **Run Container**

```bash
docker run -d \
  --name k8s-resource-watcher \
  -v $(pwd)/config.yaml:/app/config.yaml \
  -e KUBECONFIG=/root/.kube/config \
  k8s-resource-watcher:enhanced
```

## **Monitoring & Health Checks**

The application provides health check endpoints using the Gin framework:

- **`/healthz`**: Liveness probe
- **`/readyz`**: Readiness probe
- **`/`**: Application status

## **Configuration Options**

### **Enhanced Watcher Configuration**

| Option | Description | Default |
|--------|-------------|---------|
| `deploymentImportantFields` | Fields to monitor for deployment changes | Built-in production defaults |
| `eventDeduplicationWindow` | Time window for deduplication | `30s` |
| `resourceVersionCheck` | Enable resource version optimization | `true` |
| `metricsEnabled` | Enable metrics collection | `true` |

### **Environment Variables**

| Variable | Description | Example |
|----------|-------------|---------|
| `CLUSTER_NAME` | Override cluster name from config | `production-cluster` |
| `KUBECONFIG` | Path to kubeconfig file | `~/.kube/config` |

## **Configuration Examples**

### **Basic Resource Watching**
```yaml
resources:
  - kind: "ConfigMap"
    namespace: "default"  # Watch only in default namespace
  - kind: "Secret"
    namespace: ""         # Watch in all namespaces
  - kind: "Deployment"
    namespace: "prod"     # Watch deployments in prod namespace
```

### **Advanced Deployment Monitoring**
```yaml
watcher:
  deploymentImportantFields:
    - "containers"              # Container changes
    - "volumes"                 # Volume changes
    - "serviceAccountName"      # Service account changes
    - "nodeSelector"            # Node selection changes
    - "affinity"                # Affinity rules
    - "tolerations"             # Tolerations
    - "securityContext"         # Security settings
    - "imagePullSecrets"        # Image pull secrets
    - "hostAliases"             # Host aliases
    - "initContainers"          # Init container changes
    # Custom fields
    - "spec.template.spec.dnsPolicy"
```

### **Production Configuration with Enhanced Features**
```yaml
clusterName: "production-cluster"

watcher:
  deploymentImportantFields:
    - "containers"
    - "volumes"
    - "securityContext"
  eventDeduplicationWindow: "60s"    # 1 minute deduplication
  resourceVersionCheck: true         # Enable optimization
  metricsEnabled: true               # Enable metrics

resources:
  - kind: "Deployment"
    namespace: "prod"
  - kind: "ConfigMap"
    namespace: "prod"
  - kind: "Secret"
    namespace: "prod"
  - kind: "Service"
    namespace: ""               # Watch all services everywhere
```

## **Troubleshooting**

### **Common Issues**

1. **"Failed to build kubeconfig"**
   - Ensure `KUBECONFIG` environment variable is set
   - Or run from within a Kubernetes cluster

2. **"Failed to send email notification"**
   - Check SMTP settings in configuration
   - Verify network connectivity to SMTP server

3. **"No events being processed"**
   - Verify resource configuration in `config.yaml`
   - Check cluster permissions for watched resources

4. **"Duplicate notifications"**
   - Ensure no overlapping resource configurations
   - Check `eventDeduplicationWindow` setting
   - Verify single informer per resource type per namespace

### **Debug Mode**

Enable debug logging by setting log level in configuration:

```yaml
logging:
  level: "debug"
  format: "text"
```

## **Contributing**

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests if applicable
5. Submit a pull request

---

**Built with ❤️ for Kubernetes operators who want reliable and resource monitoring.**
