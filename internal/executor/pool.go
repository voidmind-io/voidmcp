// Package executor runs user-supplied JavaScript in sandboxed QuickJS WASM
// runtimes. Each execution acquires a fresh runtime from a pre-warmed pool,
// runs the script, then discards the runtime to prevent cross-session state
// leakage.
package executor

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/fastschema/qjs"
)

// Pool manages a fixed-size pool of pre-warmed QuickJS WASM runtimes.
// At most size executions run in parallel; additional callers block in Acquire
// until a slot becomes free or their context is cancelled.
//
// Runtimes are never reused across executions. Release always discards the
// used runtime and places a fresh one back into the pool to prevent JS global
// state from leaking between sessions.
type Pool struct {
	pool       chan *qjs.Runtime
	size       int
	memLimit   int    // bytes, passed to qjs.Option.MemoryLimit
	timeout    int    // milliseconds, passed to qjs.Option.MaxExecutionTime
	sandboxDir string // empty dir mounted as WASM filesystem root
}

// NewPool creates a Pool with the given concurrency limit, memory limit (in
// megabytes), and per-execution timeout. An empty sandbox directory is created
// to use as the WASM filesystem root; this prevents scripts from accessing
// the real host filesystem. The pool is pre-warmed: all size runtimes are
// created before NewPool returns.
func NewPool(size, memLimitMB int, timeout time.Duration) (*Pool, error) {
	sandboxDir, err := os.MkdirTemp("", "voidmcp-sandbox-*")
	if err != nil {
		return nil, fmt.Errorf("executor: create sandbox directory: %w", err)
	}

	p := &Pool{
		pool:       make(chan *qjs.Runtime, size),
		size:       size,
		memLimit:   memLimitMB * 1024 * 1024,
		timeout:    int(timeout.Milliseconds()),
		sandboxDir: sandboxDir,
	}

	for i := 0; i < size; i++ {
		rt, err := p.newRuntime(context.Background())
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("executor: init runtime %d/%d: %w", i+1, size, err)
		}
		p.pool <- rt
	}

	return p, nil
}

// newRuntime allocates a single QJS runtime with the pool's limits applied.
// Stdout and Stderr are discarded to prevent host process output leakage.
// CWD is set to the empty sandbox directory to prevent filesystem access.
func (p *Pool) newRuntime(ctx context.Context) (*qjs.Runtime, error) {
	rt, err := qjs.New(qjs.Option{
		CWD:                p.sandboxDir,
		Context:            ctx,
		CloseOnContextDone: true,
		MemoryLimit:        p.memLimit,
		MaxExecutionTime:   p.timeout,
		Stdout:             io.Discard,
		Stderr:             io.Discard,
	})
	if err != nil {
		return nil, err
	}
	return rt, nil
}

// Acquire blocks until a pool slot is available or ctx is cancelled. When a
// slot becomes free, the placeholder runtime is closed and a fresh runtime
// bound to ctx is created and returned. The caller must call Release when done.
func (p *Pool) Acquire(ctx context.Context) (*qjs.Runtime, error) {
	select {
	case slot := <-p.pool:
		// A nil slot means a previous Release could not create a replacement.
		// Try to recover the slot here before using it.
		if slot != nil {
			slot.Close()
		}
		rt, err := p.newRuntime(ctx)
		if err != nil {
			// Restore the slot as nil so the pool size is preserved and a
			// future Acquire can retry creating a runtime.
			p.pool <- nil
			return nil, fmt.Errorf("executor: create execution runtime: %w", err)
		}
		return rt, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Release discards the used runtime and returns a fresh placeholder to the
// pool. Runtimes are unconditionally discarded — never reused — to prevent
// cross-session JS global state leakage.
func (p *Pool) Release(rt *qjs.Runtime) {
	if rt != nil {
		rt.Close()
	}
	fresh, err := p.newRuntime(context.Background())
	if err != nil {
		// Can't create a replacement — put nil as a sentinel so the pool slot
		// is preserved. The next Acquire will detect the nil and retry.
		select {
		case p.pool <- nil:
		default:
			// Pool is full; should not occur with correct Acquire/Release pairing.
		}
		return
	}
	select {
	case p.pool <- fresh:
	default:
		// Pool is full; should not occur with correct Acquire/Release pairing.
		fresh.Close()
	}
}

// Close drains all idle runtimes from the pool, closes them, and removes the
// sandbox directory created at construction. Close must not be called
// concurrently with Acquire or Release.
func (p *Pool) Close() {
	close(p.pool)
	for rt := range p.pool {
		if rt != nil {
			rt.Close()
		}
	}
	if p.sandboxDir != "" {
		os.RemoveAll(p.sandboxDir)
	}
}

// Available returns the number of runtimes currently idle in the pool.
// The value is a snapshot and may change immediately after being read.
func (p *Pool) Available() int {
	return len(p.pool)
}
