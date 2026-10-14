package docker

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// cliProxy lets the docker CLI (buildx) reach a worker's Docker: a unix
// socket on the controller that pipes each connection to the worker over the
// same mTLS the SDK client uses (the CLI cannot do the cluster CA's checks).
// The path is fixed per worker, so a buildx builder made through it stays
// valid. The build context streams through it, over the mesh.
type cliProxy struct {
	addr, path string
	tc         *tls.Config

	mu   sync.Mutex
	refs int
	ln   net.Listener
}

func newCLIProxy(addr string, tc *tls.Config) *cliProxy {
	return &cliProxy{addr: addr, tc: tc, path: filepath.Join(os.TempDir(), "stackr-docker-"+slug(addr)+".sock")}
}

// slug is addr as a name part: "10.207.255.3:7443" is "10-207-255-3-7443".
func slug(addr string) string {
	return strings.NewReplacer(".", "-", ":", "-", "[", "", "]", "").Replace(addr)
}

func (p *cliProxy) open() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refs == 0 {
		_ = os.Remove(p.path)
		ln, err := net.Listen("unix", p.path)
		if err != nil {
			return fmt.Errorf("docker cli proxy: %w", err)
		}
		p.ln = ln
		go p.serve(ln)
	}
	p.refs++
	return nil
}

func (p *cliProxy) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refs--; p.refs == 0 {
		_ = p.ln.Close()
		_ = os.Remove(p.path)
	}
}

func (p *cliProxy) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go p.pipe(c)
	}
}

func (p *cliProxy) pipe(c net.Conn) {
	defer func() { _ = c.Close() }()
	up, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", p.addr, p.tc)
	if err != nil {
		return
	}
	defer func() { _ = up.Close() }()
	go func() {
		_, _ = io.Copy(up, c)
		_ = up.CloseWrite()
	}()
	_, _ = io.Copy(c, up)
}

// command is a docker CLI command aimed at this client's daemon. done must run
// once the command has finished.
func (d *Client) command(ctx context.Context, args ...string) (cmd *exec.Cmd, done func(), err error) {
	cmd = exec.CommandContext(ctx, "docker", args...)
	if d.px == nil {
		return cmd, func() {}, nil
	}
	if err := d.px.open(); err != nil {
		return nil, nil, err
	}
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+d.endpoint())
	return cmd, d.px.close, nil
}

func (d *Client) endpoint() string { return "unix://" + d.px.path }

// builderName is the buildx builder for this daemon: builders are named on
// the controller, so a worker's gets its address appended.
func (d *Client) builderName(name string) string {
	if d.px == nil {
		return name
	}
	return name + "-" + slug(d.px.addr)
}
