// Package cli wires the cobra commands together.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/TatsuyaOGth/camback/internal/config"
)

// Exit codes.
const (
	ExitOK       = 0
	ExitFailure  = 1
	ExitConflict = 2
	ExitCanceled = 3
)

// ExitError carries a specific exit code.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// Version is set at build time with -ldflags "-X .../cli.Version=v0.1.0".
var Version = "dev"

type app struct {
	configFlag string
	stdout     io.Writer
	stderr     io.Writer
	prompt     *termPrompter
}

// Execute runs the CLI and returns the process exit code.
func Execute(ctx context.Context, args []string) int {
	return executeWith(ctx, args, os.Stdin, os.Stdout, os.Stderr)
}

func executeWith(ctx context.Context, args []string, stdin *os.File, stdout, stderr io.Writer) int {
	a := &app{stdout: stdout, stderr: stderr, prompt: newTermPrompter(ctx, stdin, stderr)}
	root := a.rootCmd()
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}
	fmt.Fprintln(a.stderr, "Error:", err)
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	if errors.Is(err, context.Canceled) {
		return ExitCanceled
	}
	return ExitFailure
}

func (a *app) rootCmd() *cobra.Command {
	ro := &runOpts{}
	root := &cobra.Command{
		Use:   "camback",
		Short: "Back up memory cards to a NAS over SFTP",
		Long: `camback copies everything on a camera's memory card to the camera's folder
on a NAS, keeping the card's directory structure. It never overwrites files:
a changed photo or video aborts the run, any other changed file is saved
next to the old one with a timestamp.

Running "camback" without a subcommand is the same as "camback run".`,
		Version:       Version,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return a.run(cmd.Context(), ro) },
	}
	root.PersistentFlags().StringVar(&a.configFlag, "config", "",
		fmt.Sprintf("config file (default: $%s or the per-OS location)", config.EnvConfig))
	ro.register(root)

	run := &cobra.Command{
		Use:   "run",
		Short: "Detect the card, show the plan, confirm and transfer",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return a.run(cmd.Context(), ro) },
	}
	ro.register(run)

	root.AddCommand(run, a.inspectCmd())
	return root
}

// loadConfig loads the config. When optional is true a missing file is not
// an error (returns nil, path, nil).
func (a *app) loadConfig(optional bool) (*config.Config, string, error) {
	p, err := config.ResolvePath(a.configFlag)
	if err != nil {
		return nil, "", err
	}
	cfg, err := config.Load(p)
	if err != nil {
		if optional && errors.Is(err, os.ErrNotExist) {
			return nil, p, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil, p, fmt.Errorf("config file not found: %s (see config.example.toml)", p)
		}
		return nil, p, err
	}
	return cfg, p, nil
}
