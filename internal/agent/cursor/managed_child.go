package cursor

import (
	"bytes"
	"errors"
	"io"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
)

// managedChild is the stable process handle captured by the broker. It retains
// every generation until graceful close is known to have succeeded, so later
// escalation cannot orphan a replaced Cursor process.
type managedChild struct{ session *Session }

type closedWriteCloser struct{ io.Writer }

func (closedWriteCloser) Close() error { return nil }

func (child *managedChild) snapshotRuntimes() []*runtime {
	if child == nil || child.session == nil {
		return nil
	}
	child.session.mu.Lock()
	defer child.session.mu.Unlock()
	runtimes := make([]*runtime, 0, len(child.session.owned)+len(child.session.retired)+1)
	seen := make(map[*runtime]struct{}, len(child.session.owned)+len(child.session.retired)+1)
	for rt := range child.session.owned {
		runtimes = append(runtimes, rt)
		seen[rt] = struct{}{}
	}
	// Keep compatibility with low-level fixtures that predate the ownership set.
	// Production sessions use a non-nil map, including after every child is reaped.
	if child.session.owned == nil {
		for _, rt := range append(append([]*runtime(nil), child.session.retired...), child.session.rt) {
			if _, exists := seen[rt]; !exists {
				runtimes = append(runtimes, rt)
				seen[rt] = struct{}{}
			}
		}
	}
	return runtimes
}

func (child *managedChild) snapshot() []provider.ManagedChild {
	runtimes := child.snapshotRuntimes()
	result := make([]provider.ManagedChild, 0, len(runtimes))
	for _, rt := range runtimes {
		if rt != nil && !common.IsNil(rt.child) {
			result = append(result, rt.child)
		}
	}
	return result
}

func (child *managedChild) current() provider.ManagedChild {
	if child == nil || child.session == nil {
		return nil
	}
	child.session.mu.Lock()
	defer child.session.mu.Unlock()
	if child.session.rt == nil {
		return nil
	}
	return child.session.rt.child
}

func (child *managedChild) Input() io.WriteCloser {
	if current := child.current(); !common.IsNil(current) {
		if stream := current.Input(); stream != nil {
			return stream
		}
	}
	return closedWriteCloser{Writer: io.Discard}
}
func (child *managedChild) Output() io.Reader {
	if current := child.current(); !common.IsNil(current) {
		if stream := current.Output(); stream != nil {
			return stream
		}
	}
	return bytes.NewReader(nil)
}
func (child *managedChild) Errors() io.Reader {
	if current := child.current(); !common.IsNil(current) {
		if stream := current.Errors(); stream != nil {
			return stream
		}
	}
	return bytes.NewReader(nil)
}
func (child *managedChild) Wait() error {
	var joined error
	for _, rt := range child.snapshotRuntimes() {
		if rt == nil || common.IsNil(rt.child) {
			continue
		}
		err := rt.child.Wait()
		joined = errors.Join(joined, err)
		if err == nil {
			child.session.releaseRuntime(rt)
		}
	}
	return joined
}
func (child *managedChild) Terminate() error {
	var err error
	for _, owned := range child.snapshot() {
		err = errors.Join(err, owned.Terminate())
	}
	return err
}
func (child *managedChild) Kill() error {
	var err error
	for _, owned := range child.snapshot() {
		err = errors.Join(err, owned.Kill())
	}
	return err
}

var _ provider.ManagedChild = (*managedChild)(nil)
