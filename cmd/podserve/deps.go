package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/podserve/podserve/pkg/hf"
)

// defaultDeps is the real outside world. Spine file (P01 T1).
func defaultDeps() deps {
	return deps{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Getenv: os.Getenv,
		NewHF:  func(token string) HFClient { return hf.New(token) },
		NewDynamic: func() (dynamic.Interface, error) {
			rules := clientcmd.NewDefaultClientConfigLoadingRules()
			cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
			if err != nil {
				return nil, err
			}
			return dynamic.NewForConfig(cfg)
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
