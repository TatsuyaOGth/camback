// Command camback backs up camera memory cards to a NAS over SFTP.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/TatsuyaOGth/camback/internal/cli"
)

func main() {
	// The first Ctrl-C cancels the context: prompts return, planning stops,
	// and a transfer stops between reads and removes its temporary file.
	// A second Ctrl-C quits immediately, in case something is still blocked
	// (e.g. a slow network call).
	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		cancel()
		fmt.Fprintln(os.Stderr, "\nInterrupted, stopping... (press Ctrl-C again to quit immediately)")
		<-sig
		os.Exit(cli.ExitCanceled)
	}()

	code := cli.Execute(ctx, os.Args[1:])
	cancel()
	os.Exit(code)
}
