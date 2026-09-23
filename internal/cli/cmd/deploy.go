package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/FyrmForge/stackr/internal/deploystate"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/FyrmForge/stackr/internal/cli"
)

func newLogsCmd(rt *Runtime) *cobra.Command {
	var follow bool
	var tail int
	cmd := &cobra.Command{
		Use:   "logs [tile]",
		Short: "Show (or stream) a tile's logs",
		Long: `Show the last --tail lines of a tile's logs, or stream them live with
--follow. Under --json, --follow emits NDJSON events:
{"stream":"stdout"|"stderr","line":"..."} per line.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			ref := ""
			if len(args) > 0 {
				ref = args[0]
			}
			app, err := resolveApp(cmd.Context(), rt, client, ref)
			if err != nil {
				return err
			}
			if follow {
				err := client.FollowLogs(cmd.Context(), app.ID, tail, func(ev cli.LogEvent) error {
					if rt.JSON {
						b, _ := json.Marshal(ev)
						_, werr := fmt.Fprintln(rt.Stdout, string(b))
						return werr
					}
					_, werr := fmt.Fprintln(rt.Stdout, ev.Line)
					return werr
				})
				if errors.Is(err, context.Canceled) {
					return nil // Ctrl-C on a stream is a clean stop
				}
				return err
			}
			out, err := client.Logs(cmd.Context(), app.ID, tail)
			if err != nil {
				return err
			}
			if rt.JSON {
				lines := []string{}
				if out != "" {
					lines = strings.Split(strings.TrimRight(out, "\n"), "\n")
				}
				return rt.EmitJSON(lines)
			}
			if out == "" {
				_, _ = fmt.Fprintln(rt.Stdout, "(no logs)")
				return nil
			}
			_, _ = fmt.Fprint(rt.Stdout, out)
			if !strings.HasSuffix(out, "\n") {
				_, _ = fmt.Fprintln(rt.Stdout)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&follow, "follow", false, "stream live logs")
	cmd.Flags().IntVar(&tail, "tail", 200, "number of lines to show")
	return cmd
}

// deployResult is the JSON output of `deploy`.
type deployResult struct {
	DeploymentID string `json:"deployment_id"`
	App          string `json:"app"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
}

func newDeployCmd(rt *Runtime) *cobra.Command {
	var noWait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "deploy [tile]",
		Short: "Trigger a deployment and wait for it to finish",
		Long: `Trigger a deployment. By default the command waits, polling the deployment
until it reaches a terminal state (done, error, cancelled). Ctrl-C detaches,
it stops waiting, it does not cancel the deployment. --no-wait returns
immediately with the deployment id. A tile gated on CI (wait_for_ci) can sit
in waiting_ci indefinitely; bound that with --timeout.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			ref := ""
			if len(args) > 0 {
				ref = args[0]
			}
			app, err := resolveApp(cmd.Context(), rt, client, ref)
			if err != nil {
				return err
			}
			dep, err := client.Deploy(cmd.Context(), app.ID)
			if err != nil {
				return err
			}
			if noWait {
				if rt.JSON {
					return rt.EmitJSON(deployResult{dep, app.Name, "queued", ""})
				}
				_, _ = fmt.Fprintf(rt.Stdout, "Deploying %s (deployment %s)\n", app.Name, dep)
				return nil
			}
			if !rt.JSON {
				_, _ = fmt.Fprintf(rt.Stderr, "Deploying %s (deployment %s)…\n", app.Name, dep)
			}
			d, err := waitForDeployment(cmd.Context(), client, dep, timeout, rt)
			if errors.Is(err, context.Canceled) {
				// Detach: the deployment keeps running server-side.
				_, _ = fmt.Fprintf(rt.Stderr, "Detached. Deployment %s continues. Check it with `stackr deploy --no-wait` output or the panel.\n", dep)
				return nil
			}
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(deployResult{d.ID, app.Name, d.Status, d.Error})
			}
			switch d.Status {
			case "done":
				_, _ = fmt.Fprintf(rt.Stdout, "Deployed %s (deployment %s)\n", app.Name, d.ID)
				return nil
			default:
				msg := d.Status
				if d.Error != "" {
					msg += ": " + d.Error
				}
				return fmt.Errorf("deployment %s %s", d.ID, msg)
			}
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return immediately after queuing")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "give up waiting after this long (0 = wait forever)")
	return cmd
}

// waitForDeployment polls until the deployment is terminal. Backoff 1s → 5s.
func waitForDeployment(ctx context.Context, client *cli.Client, id string, timeout time.Duration, rt *Runtime) (cli.Deployment, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	delay := time.Second
	last := ""
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	tick := 0
	start := time.Now()
	// a \r spinner on stderr, not a bubbletea program, one line of
	// state doesn't need an event loop.
	progress := func(status string) {
		if rt.JSON {
			return
		}
		if rt.StderrTTY {
			_, _ = fmt.Fprintf(rt.Stderr, "\r%s %s (%ds) ", frames[tick%len(frames)], status, int(time.Since(start).Seconds()))
			tick++
			return
		}
		if status != last {
			_, _ = fmt.Fprintf(rt.Stderr, "  %s\n", status)
			last = status
		}
	}
	clearLine := func() {
		if !rt.JSON && rt.StderrTTY {
			_, _ = fmt.Fprint(rt.Stderr, "\r\033[K")
		}
	}
	for {
		d, err := client.GetDeployment(ctx, id)
		if err != nil {
			clearLine()
			if ctx.Err() != nil {
				return cli.Deployment{}, ctx.Err()
			}
			return cli.Deployment{}, err
		}
		progress(d.Status)
		if deploystate.IsTerminal(d.Status) {
			clearLine()
			return d, nil
		}
		select {
		case <-ctx.Done():
			clearLine()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return cli.Deployment{}, fmt.Errorf("timed out waiting for deployment %s (still %s); it continues server-side", id, d.Status)
			}
			return cli.Deployment{}, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 5*time.Second {
			delay += time.Second
		}
	}
}

func newForwardCmd(rt *Runtime) *cobra.Command {
	var portSpec, address string
	cmd := &cobra.Command{
		Use:   "forward [tile]",
		Short: "Tunnel a local port to a tile's container port",
		Long: `Tunnel a local port to a tile's container port, kubectl-style. --port takes
[local:]remote; a bare number is the local port only, and with no --port the
server picks the tile's own port. Runs until Ctrl-C.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if rt.JSON {
				return usagef("forward has no machine-readable output; drop --json")
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			ref := ""
			if len(args) > 0 {
				ref = args[0]
			}
			id, name, err := resolveTile(cmd.Context(), rt, client, ref)
			if err != nil {
				return err
			}
			local, remote, err := parsePorts(portSpec)
			if err != nil {
				return usagef("%v", err)
			}
			ctx := cmd.Context()

			// The session websocket, held until exit: it puts this forward on
			// the canvas and doubles as the startup probe, a missing scope,
			// stopped tile or bad port fails here. It also carries back the
			// port the server resolved.
			presence, resolved, err := client.ForwardPresence(ctx, id, remote)
			if err != nil {
				return err
			}
			remote = resolved
			go keepPresence(ctx, client, id, remote, presence)
			if local == 0 {
				local = remote
			}

			ln, err := net.Listen("tcp", net.JoinHostPort(address, strconv.Itoa(local)))
			if err != nil {
				return err
			}
			// Signal-context cancellation alone won't unblock Accept, close
			// the listener when the context ends.
			go func() {
				<-ctx.Done()
				_ = ln.Close()
			}()
			defer func() { _ = ln.Close() }()
			_, _ = fmt.Fprintf(rt.Stdout, "Forwarding %s -> %s:%d   (Ctrl-C to stop)\n", ln.Addr(), name, remote)

			for {
				conn, err := ln.Accept()
				if err != nil {
					return nil // listener closed: clean stop
				}
				go func() {
					defer func() { _ = conn.Close() }()
					tunnel, _, err := client.Forward(ctx, id, remote)
					if err != nil {
						_, _ = fmt.Fprintf(rt.Stderr, "forward: %v\n", err)
						return
					}
					defer func() { _ = tunnel.Close() }()
					// Wait on the reply direction, not on whichever finishes
					// first: psql, curl and `nc -N` all stop sending before
					// the answer arrives, and returning there closed the
					// tunnel mid-response.
					go func() { _, _ = io.Copy(tunnel, conn) }()
					_, _ = io.Copy(conn, tunnel)
				}()
			}
		},
	}
	cmd.Flags().StringVar(&portSpec, "port", "", "[local:]remote port")
	cmd.Flags().StringVar(&address, "address", "127.0.0.1", "local address to bind")
	return cmd
}

// keepPresence re-registers the forward's canvas row whenever the server
// bounces: a restart drops every presence websocket, and without this the
// forward keeps working (data tunnels are per-connection) but silently
// disappears from the canvas. Reconnects retry until the context ends.
func keepPresence(ctx context.Context, client *cli.Client, id string, remote int, conn net.Conn) {
	for {
		_, _ = io.Copy(io.Discard, conn) // blocks until the session drops
		_ = conn.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			next, _, err := client.ForwardPresence(ctx, id, remote)
			if err == nil {
				conn = next
				break
			}
		}
	}
}

// parsePorts reads kubectl's "[local:]remote" syntax. A bare number is the
// local port only; 0 means "let the server pick the tile's own".
func parsePorts(s string) (local, remote int, err error) {
	if s == "" {
		return 0, 0, nil
	}
	l, r, split := strings.Cut(s, ":")
	if !split {
		if local, err = parsePort(l); err != nil {
			return 0, 0, err
		}
		return local, 0, nil
	}
	if local, err = parsePort(l); err != nil {
		return 0, 0, err
	}
	if remote, err = parsePort(r); err != nil {
		return 0, 0, err
	}
	return local, remote, nil
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return n, nil
}
