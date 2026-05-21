# Argo Workflows Integration Example

## Prerequisites

```bash
# Install Argo
kubectl create namespace argo
kubectl apply -n argo -f https://github.com/argoproj/argo-workflows/releases/latest/download/install.yaml

# Create AWS credentials secret
kubectl create secret generic aws-credentials \
    --from-file=credentials=$HOME/.aws/credentials \
    --from-file=config=$HOME/.aws/config
```

## Running

```bash
# Submit workflow
argo submit spawn-sweep-workflow.yaml

# Watch progress
argo watch @latest

# Get logs
argo logs @latest
```

## See Also

- [Argo Documentation](https://argoproj.github.io/workflows/)
- [spawn WORKFLOW_INTEGRATION.md](../../../WORKFLOW_INTEGRATION.md)
