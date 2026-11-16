package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/jonas-budde/istio-sidecar-sweeper/internal/sweeper"
)

var version = "dev"

const usage = `kubectl istio-sweeper finds pods whose Istio sidecar is out of date and restarts their workloads.

Usage:
  kubectl istio-sweeper scan    [flags]   list pods with an outdated sidecar
  kubectl istio-sweeper restart [flags]   restart their workloads (dry run unless --apply)

Flags:
`

var errFound = errors.New("outdated sidecars or warnings found")

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, errFound) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stderr, usage)
		newFlags("").PrintDefaults()
		return nil
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Println(version)
		return nil
	}
	cmd := args[0]
	if cmd != "scan" && cmd != "restart" {
		return fmt.Errorf("unknown command %q", cmd)
	}
	fs := newFlags(cmd)
	var (
		kubeconfig = fs.String("kubeconfig", "", "path to kubeconfig (default: standard loading rules)")
		kctx       = fs.String("context", "", "kubeconfig context")
		namespace  = fs.String("namespace", "", "only this namespace (default: all)")
		istioNS    = fs.String("istio-namespace", "", "namespace of the Istio control plane (default: auto-detect aks-istio-system, then istio-system)")
		exclude    = fs.String("exclude-namespaces", "kube-system,aks-istio-system,istio-system", "comma-separated namespaces to ignore")
		exitCode   = fs.Bool("exit-code", false, "scan: exit with status 2 if outdated sidecars or warnings are found (for CI)")
		apply      = fs.Bool("apply", false, "restart: really restart (default is a dry run)")
		single     = fs.Bool("allow-single-replica", false, "restart: also restart single-replica workloads")
		timeout    = fs.Duration("timeout", 5*time.Minute, "restart: rollout timeout per workload")
	)
	fs.StringVar(namespace, "n", "", "shorthand for --namespace")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = *kubeconfig
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: *kctx}).ClientConfig()
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if *istioNS == "" {
		if *istioNS, err = sweeper.DetectIstioNamespace(ctx, cs); err != nil {
			return err
		}
	}
	stale, warnings, err := sweeper.Scan(ctx, cs, *istioNS, *namespace, strings.Split(*exclude, ",")...)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	if len(stale) == 0 {
		fmt.Println("No outdated sidecars found.")
		if *exitCode && len(warnings) > 0 {
			return errFound
		}
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tPOD\tHAVE\tWANT\tWORKLOAD")
	for _, s := range stale {
		fmt.Fprintf(w, "%s\t%s\t%s (%s)\t%s (%s)\t%s/%s\n", s.Namespace, s.Name, s.Have, s.HaveRev, s.Want, s.WantRev, s.Owner.Kind, s.Owner.Name)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if cmd == "scan" {
		if *exitCode {
			return errFound
		}
		return nil
	}

	fmt.Println()
	res, err := sweeper.Restart(ctx, cs, sweeper.Workloads(stale), sweeper.Options{Apply: *apply, Timeout: *timeout, AllowSingle: *single})
	for _, r := range res {
		fmt.Printf("%-14s %s %s\n", r.Action, r.Workload, r.Reason)
	}
	if !*apply {
		fmt.Println("\nDry run. Use --apply to restart.")
	}
	return err
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}
