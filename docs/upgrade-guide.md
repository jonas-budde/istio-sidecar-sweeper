# AKS Istio add-on upgrade guide (with istio-sidecar-sweeper)

After an Istio add-on upgrade, running pods keep their old sidecar until they are
restarted. This guide shows the canary upgrade flow and where the sweeper saves work.

Everything below was exercised on an AKS cluster (1.36, westeurope) with the Istio add-on
(asm-1-29 → asm-1-30). Patch updates were only simulated (see the end).

## 1. Check the current state

```bash
az aks show -g <rg> -n <cluster> --query serviceMeshProfile.istio.revisions
az aks mesh get-revisions -l <location> -o table   # shows compatible upgrade targets
kubectl istio-sweeper scan                          # baseline: should be clean
```

## 2. Start the canary upgrade

```bash
az aks mesh upgrade start -g <rg> -n <cluster> --revision asm-1-30
```

Both control planes (old and new revision) now run side by side. Nothing changes for
existing pods yet.

## 3. Move namespaces to the new revision

```bash
kubectl label namespace <ns> istio.io/rev=asm-1-30 --overwrite
```

Pods still run the old sidecar until restarted.

## 4. Find and restart stale pods

Without the tool: look up every pod's sidecar version by hand and run
`kubectl rollout restart` per Deployment, StatefulSet and DaemonSet in each labeled namespace.

With the tool:

```bash
kubectl istio-sweeper scan                 # lists pods whose sidecar differs from the injector
kubectl istio-sweeper restart              # dry run: shows what would be restarted
kubectl istio-sweeper restart -apply       # rolling restart, one workload at a time
```

Safety behavior:

- Waits for each rollout before the next workload.
- Skips workloads that are not fully ready (also in dry run).
- Skips single-replica workloads unless `-allow-single-replica` is set.
- Excludes `kube-system` and `aks-istio-system` by default on AKS (`-provider azure`, `-exclude-namespaces` overrides).

## 5. Verify

```bash
kubectl istio-sweeper scan -exit-code   # exit 0 = clean, 2 = stale pods or warnings, 1 = error
```

The same command works as a CI/pipeline gate.

## 6. Complete or roll back

```bash
az aks mesh upgrade complete -g <rg> -n <cluster> --yes   # removes the old revision
# or
az aks mesh upgrade rollback -g <rg> -n <cluster>
```

Only complete once the scan is clean: workloads still on the old revision lose their
control plane. If a namespace still points at a removed revision, the scan warns about it
(this case is documented from the CLI help, not observed).

`complete` asks for interactive confirmation. Without a TTY (CI, scripts) it fails with
`NoTTYException` and prints "To check existing issues…". Pass `--yes` in automation
(tested: exit 0, only `asm-1-30` remained afterwards).

## Patch updates

Patch versions are rolled out by AKS into the injector configuration of the same
revision. Pods keep the old patch sidecar until restarted, so `scan` reports them and
`restart -apply` fixes them. This was simulated by editing the injector ConfigMap tag
(1.30.7-1 → 1.30.7-6), not observed in a real add-on patch rollout.

## Limitations

- `restart` uses a rollout restart, which does not consult PodDisruptionBudgets.
- Pods with a custom proxy image annotation may stay stale after a restart.
- Only Deployments, StatefulSets and DaemonSets are handled.
