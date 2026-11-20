package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ryanymt/podserve/pkg/hf"
	"github.com/ryanymt/podserve/pkg/kube"
)

// defaultDeps is the real outside world. Spine file (P01 T1; P02 T2 adds the shared rest.Config, the
// log reader and the warning filter).
func defaultDeps() deps {
	return deps{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Getenv: os.Getenv,
		NewHF:  func(token string) HFClient { return hf.New(token) },
		NewDynamic: func() (dynamic.Interface, error) {
			cfg, err := restConfig(os.Stderr)
			if err != nil {
				return nil, err
			}
			return dynamic.NewForConfig(cfg)
		},
		NewLogReader: func() (kube.LogReader, error) {
			cfg, err := restConfig(os.Stderr)
			if err != nil {
				return nil, err
			}
			return kube.NewLogReader(cfg)
		},
		Exec: func(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Stdout, cmd.Stderr, cmd.Stdin = stdout, stderr, os.Stdin
			return cmd.Run()
		},
		Now:     time.Now,
		Sleep:   time.Sleep,
		Version: version,
	}
}

// restConfig is the client config for the current kubeconfig context, shared by the dynamic client
// and the log reader, with D-033's warning filter installed.
func restConfig(stderr io.Writer) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg.WarningHandler = autopilotWarnings{w: stderr}
	return cfg, nil
}

// autopilotWarnings drops GKE Autopilot's admission warnings (D-033). Autopilot adds tolerations and
// default resources to every GPU pod and says so on each apply ("autopilot-workload-defaulter:...",
// "autopilot-default-resources-mutator:..."); they are expected and harmless. Every other warning
// still reaches the user, as client-go would print it.
type autopilotWarnings struct{ w io.Writer }

func (h autopilotWarnings) HandleWarningHeader(code int, _ string, text string) {
	if code != 299 || text == "" || strings.HasPrefix(text, "autopilot-") {
		return
	}
	fmt.Fprintf(h.w, "Warning: %s\n", text)
}
