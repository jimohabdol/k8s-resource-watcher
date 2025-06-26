#!/bin/bash

echo "Testing resource watcher startup..."

# Set environment variables for testing
export CLUSTER_NAME="test-cluster"
export SMTP_HOST="localhost"
export SMTP_PORT="1025"
export FROM_EMAIL="test@example.com"
export TO_EMAILS="test@example.com"

# Start the application with a timeout
timeout 30s ./resource-watcher --config test-config.yaml --port 8081 &
APP_PID=$!

# Wait a moment for the app to start
sleep 5

# Check if the app is running
if ps -p $APP_PID > /dev/null; then
    echo "Application started successfully"
    
    # Test health endpoints
    echo "Testing health endpoint..."
    curl -s http://localhost:8081/healthz
    echo ""
    
    echo "Testing readiness endpoint..."
    curl -s http://localhost:8081/readyz
    echo ""
    
    # Stop the application
    echo "Stopping application..."
    kill $APP_PID
    wait $APP_PID 2>/dev/null
    echo "Application stopped"
else
    echo "Application failed to start"
    exit 1
fi 