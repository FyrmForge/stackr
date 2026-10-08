package main

import (
	"encoding/json"
	"net/url"
	"os"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// ssh opens a shell in a replica over the API's terminal websocket: binary
// frames are the terminal's bytes both ways, a text {"type":"resize"} goes
// up when the window changes, and the last text frame {"type":"exit"} down
// carries the shell's exit code.
func (a *app) ssh() *cobra.Command {
	var container, shell string
	c := leaf("ssh [tile]", "tile.terminal", "Open a shell in a replica", upTo(1),
		a.at(atTile, func(_ *cobra.Command, p string, _ []string) error {
			if a.json {
				return usage("ssh is interactive; --json does not apply")
			}
			if !a.tty {
				return usage("ssh needs a terminal; use stackr tile exec")
			}
			q := url.Values{}
			if container != "" {
				q.Set("container", container)
			}
			if shell != "" {
				q.Set("shell", shell)
			}
			ws, _, err := a.dial(p+"/terminal", q)
			if err != nil {
				return err
			}
			defer func() { _ = ws.CloseNow() }()
			ws.SetReadLimit(1 << 20)
			fd := int(os.Stdin.Fd())
			old, err := term.MakeRaw(fd)
			if err != nil {
				return err
			}
			defer func() { _ = term.Restore(fd, old) }()

			resize := func() {
				if cols, rows, err := term.GetSize(fd); err == nil {
					b, _ := json.Marshal(map[string]any{"type": "resize", "cols": cols, "rows": rows})
					_ = ws.Write(a.ctx, websocket.MessageText, b)
				}
			}
			resize()
			watchWinch(a.ctx, resize)
			go func() { // keys to the shell
				buf := make([]byte, 4096)
				for {
					n, err := a.in.Read(buf)
					if n > 0 && ws.Write(a.ctx, websocket.MessageBinary, buf[:n]) != nil {
						return
					}
					if err != nil {
						_ = ws.Close(websocket.StatusNormalClosure, "")
						return
					}
				}
			}()
			code, exited := 0, false
			for {
				typ, b, err := ws.Read(a.ctx)
				if err != nil {
					if exited || a.ctx.Err() != nil {
						break
					}
					return err
				}
				if typ == websocket.MessageBinary {
					if _, err := a.out.Write(b); err != nil {
						return err
					}
					continue
				}
				var m struct {
					Type string
					Code int
				}
				if json.Unmarshal(b, &m) == nil && m.Type == "exit" {
					code, exited = m.Code, true
				}
			}
			if code != 0 {
				return exitErr(code)
			}
			return nil
		}))
	c.Flags().StringVar(&container, "container", "", "the replica (default: a running one)")
	c.Flags().StringVar(&shell, "shell", "", "the shell binary (default: bash, else sh)")
	return scoped(c, true)
}
