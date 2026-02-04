# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Rules

- **Always use kubebuilder CLI to scaffold controllers.** Never manually create controller files. Use:
  ```bash
  # For controllers with new CRDs:
  kubebuilder create api --group <group> --version <version> --kind <Kind>

  # For controllers watching external CRDs:
  kubebuilder create api \
    --group <group> \
    --version <version> \
    --kind <Kind> \
    --controller=true \
    --resource=false \
    --external-api-domain <domain> \
    --external-api-path <import-path>
  ```
  Then implement your logic in the scaffolded `*_controller.go` file.

## What is OpenKruise Controller?

A Kubernetes controller that integrates OpenKruise rollout capabilities with Kuberik's rollout management system. It manages RolloutTest and RolloutStepGate resources for progressive delivery testing.

## Common Development Commands

```bash
# Generate code after modifying CRD types
make manifests              # Generate CRD YAML from Go types
make generate               # Generate DeepCopy methods

# Code quality
make fmt                    # go fmt
make vet                    # go vet
make lint                   # gofmt + govet

# Testing
make test                   # Run unit tests (Ginkgo/Gomega)
make test-e2e              # Run e2e tests on Kind cluster

# Building
make build                  # Build binary
make docker-build           # Build container image
make docker-push            # Push to registry

# Deployment
make install                # Install CRDs to cluster
make deploy                 # Deploy controller to cluster
make uninstall              # Remove CRDs and controller
make run                    # Run locally (without cluster deployment)
```

## Development Workflow

1. Modify CRD type definitions in `api/v1alpha1/*_types.go`
2. Run `make manifests generate` to update CRDs and DeepCopy methods
3. Implement reconciliation logic in `internal/controller/*_controller.go`
4. Write tests in `internal/controller/*_controller_test.go`
5. Run `make test` to verify
6. Deploy with `make docker-build docker-push deploy IMG=registry/image:tag`

## CRDs Managed

- **RolloutTest**: Defines test configurations for progressive delivery
- **RolloutStepGate**: Controls step-by-step progression in rollouts

## Dependencies

The controller depends on:
- `sigs.k8s.io/controller-runtime` - Kubebuilder framework
- OpenKruise APIs for rollout management

## Debugging

```bash
# Controller logs
kubectl logs -n kuberik-system deployment/openkruise-controller -f

# Events
kubectl get events --sort-by='.lastTimestamp'

# Describe resources
kubectl describe rollouttest <name>
kubectl describe rolloutstepgate <name>
```
