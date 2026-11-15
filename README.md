# istio-sidecar-sweeper

A kubectl plugin that finds pods whose Istio sidecar is out of date and restarts their workloads. Built for the AKS Istio add-on, where security patches and canary revision upgrades only reach running pods after a restart.

Status: early prototype.

> **Note:** This tool only matters for sidecar mode. The Istio project is moving toward ambient mode (no sidecars, so nothing to restart), and AKS offers Azure Kubernetes Application Network (AppNet), currently in preview ([docs](https://learn.microsoft.com/en-us/azure/application-network/get-started)). The docs state that a cluster with the Istio add-on enabled can't join an Application Network, so moving over means leaving the add-on. New setups should look at AppNet first; this tool is for clusters that stay on the Istio add-on with sidecars. Upstream ambient GA is from Istio 1.24 (my recollection, not re-checked here), and Microsoft has not announced an end date for the sidecar add-on that I know of.

## How it decides a pod is stale

For every namespace with an `istio.io/rev` label (or `istio-injection=enabled`), it reads the `istio-sidecar-injector-<revision>` ConfigMap in `aks-istio-system` and takes the proxy image from `global.proxy.image`. A pod is stale if its `istio-proxy` image tag, or the revision in its `istio.io/rev` annotation, differs from that.

## Install

Download the archive for your platform from the [releases page](https://github.com/jonas-budde/istio-sidecar-sweeper/releases), extract `kubectl-istio_sweeper` and put it on your `PATH`. Then run `kubectl istio-sweeper`. Krew support is planned, not available yet.

## Usage

```
go build -o kubectl-istio_sweeper ./cmd/kubectl-istio_sweeper

kubectl-istio_sweeper scan                       # list stale pods
kubectl-istio_sweeper restart -namespace demo    # dry run
kubectl-istio_sweeper restart -namespace demo -apply
```

CI: `kubectl-istio_sweeper scan -exit-code` exits with status 2 if outdated sidecars or warnings are found (0 if clean, 1 on errors).

A namespace whose revision no longer has an injector ConfigMap (for example after an upgrade removed the previous revision, based on the `az aks mesh upgrade complete` help text; I did not observe it) is reported as a warning and skipped, instead of aborting the scan.

Flags: `-kubeconfig`, `-exit-code`, `-context`, `-namespace`, `-istio-namespace` (default `aks-istio-system`), `-apply`, `-allow-single-replica`, `-timeout`, `-exclude-namespaces` (default `kube-system,aks-istio-system`), `version`.

Restart behavior:
- Deployments, StatefulSets and DaemonSets are restarted one at a time via the pod template annotation (same as `kubectl rollout restart`) and the tool waits for the rollout.
- A workload is only restarted when all its replicas are ready (checked first, also in the dry run).
- Single-replica workloads are skipped unless `-allow-single-replica` is set. Bare pods, Jobs and other owners are skipped.
- This relies on the workload's own rollout strategy and probes. It does not evict pods, so PodDisruptionBudgets are not consulted.

## Upgrade guide

Step-by-step AKS canary upgrade with this tool: [docs/upgrade-guide.md](docs/upgrade-guide.md).

## Tested

- Unit tests with a fake clientset: `go test ./...`
- AKS 1.36 with the Istio add-on, `asm-1-29` to `asm-1-30` canary upgrade, namespace relabeled to `asm-1-30`: pods on `v1.29.10-1` reported, dry run, real restart of a 2-replica Deployment, single-replica skip and forced restart all behaved as described.
- Same setup directly on `asm-1-30`: Deployment, StatefulSet (2 replicas) and DaemonSet (2 nodes) were all detected and restarted, and the scan was clean afterwards.
- Patch-level update **simulated**: I edited the `istio-sidecar-injector-asm-1-30` ConfigMap (tag `v1.30.7-1` to `v1.30.7-6`), so new pods were injected with the new image. This mimics a patch rollout by the add-on, but it is not a real one.
- The readiness pre-check, namespace exclusion, removed-revision warning and `-exit-code` are covered by unit tests only, not run on a cluster.

## Notes

- On AKS the sidecar is a native sidecar (an init container named `istio-proxy`), and the revision is in the `istio.io/rev` pod annotation, not a label.
