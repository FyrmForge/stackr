package workerclient

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/FyrmForge/stackr/internal/worker"
)

type rec struct {
	mu    sync.Mutex
	calls []string
}

func (r *rec) Run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return nil, nil
}

func TestClientRoundTrip(t *testing.T) {
	dir, _ := os.MkdirTemp("", "wc")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "d.sock")
	ln, _ := net.Listen("unix", sock)
	d := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ServerVersion":"29.1","DockerRootDir":"`+dir+`"}`)
	})}
	go d.Serve(ln)
	defer d.Close()

	r := &rec{}
	ts := httptest.NewTLSServer(worker.New(worker.Config{DockerSock: sock, StateDir: t.TempDir(), Runner: r}).Handler())
	defer ts.Close()
	c := New(ts.Listener.Addr().String(), ts.Client().Transport.(*http.Transport).TLSClientConfig)
	ctx := context.Background()

	f, err := c.Facts(ctx)
	if err != nil || f.Docker != "29.1" {
		t.Fatalf("%+v %v", f, err)
	}
	if err := c.ApplyIPTables(ctx, "*nat\nCOMMIT\n"); err != nil {
		t.Fatal(err)
	}
	st := worker.State{Chains: "*filter\nCOMMIT\n"}
	if err := c.PutState(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetState(ctx)
	if err != nil || got.Chains != st.Chains {
		t.Fatalf("%+v %v", got, err)
	}
	if err := c.Replay(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("%v", r.calls)
	}
	if err := c.ApplyWG(ctx, []byte("{}")); err == nil || !strings.Contains(err.Error(), "501") {
		t.Fatalf("want 501, got %v", err)
	}
	if o := c.DockerOptions(); o.Host != "tcp://"+ts.Listener.Addr().String() || o.HTTPClient == nil {
		t.Fatalf("%+v", o)
	}
}
