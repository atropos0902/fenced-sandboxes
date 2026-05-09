// Package provider is the seam between the lifecycle code and whoever runs the
// sandboxes. Everything above it assumes a create call can time out and still
// have worked, so every provider must be able to find a sandbox by our name.
package provider

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrNotFound = errors.New("sandbox not found")

type Spec struct {
	Name, RunID, Image string
	Epoch              int
	MemoryMB           int
	CPUs               float64
	PIDs               int64
	TTL                time.Duration // the provider kills it even if we are down
}

type Info struct {
	Name, Ref string
	Running   bool // false for stopped, paused or a leftover with no sandbox in it
	Age       time.Duration
}

type Cmd struct {
	Shell   string
	Env     map[string]string
	Dir     string
	Timeout time.Duration
}

type Result struct {
	Exit     int
	Out, Err string // tails
	TimedOut bool
	OOM      bool
}

type Endpoint struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

type Provider interface {
	Name() string
	// Create must be safe to repeat with the same Spec.Name.
	Create(ctx context.Context, s Spec) (ref string, err error)
	Exec(ctx context.Context, ref string, c Cmd) (Result, error)
	Endpoint(ctx context.Context, ref string, port int) (Endpoint, error)
	// List returns every sandbox this installation owns, paused ones included.
	List(ctx context.Context) ([]Info, error)
	// Destroy of something already gone is not an error.
	Destroy(ctx context.Context, name, ref string) error
}

// Snapshotter is optional. Blaxel keeps standby state per sandbox and has no
// call to start many sandboxes from one, so it does not implement this.
type Snapshotter interface {
	Snapshot(ctx context.Context, ref, key string) (image string, err error)
	DropSnapshot(ctx context.Context, image string) error
}

const TailBytes = 32 << 10

// Tail keeps the last max bytes written to it.
type Tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *Tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*TailBytes {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-TailBytes:]...)
	}
	return len(p), nil
}

func (t *Tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.buf
	if len(b) > TailBytes {
		b = b[len(b)-TailBytes:]
	}
	return string(b)
}
