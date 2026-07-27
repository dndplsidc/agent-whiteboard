//go:build darwin || linux

package processgroup

import (
	"errors"
	"io"
	"os"
	"sync"
)

// ErrOutputOverflow reports that a caller did not consume a child output
// stream before its bounded unread backlog filled. Bytes through the backlog
// limit remain readable; later bytes from that stream are discarded.
var ErrOutputOverflow = errors.New("process output unread backlog exceeded")

// Each stream retains at most 1 MiB that has not yet been consumed.
const maxUnreadOutputBytes = 1 << 20
const outputDrainChunkBytes = 32 << 10

// outputSpool continuously drains one child pipe so process completion never
// depends on a caller reading Output or Errors. It retains a bounded,
// in-memory-only unread prefix and reclaims bytes as a live reader consumes
// them.
type outputSpool struct {
	source *os.File
	done   chan struct{}

	mu           sync.Mutex
	ready        *sync.Cond
	buffer       []byte
	start        int
	size         int
	overflow     bool
	readerActive bool
	forceDrain   bool
	readErr      error
}

func newOutputSpool(source *os.File) *outputSpool {
	spool := &outputSpool{source: source, done: make(chan struct{})}
	spool.ready = sync.NewCond(&spool.mu)
	go spool.drain()
	return spool
}

func (spool *outputSpool) drain() {
	defer close(spool.done)
	defer spool.source.Close()

	chunk := make([]byte, outputDrainChunkBytes)
	for {
		count, err := spool.source.Read(chunk)
		if count > 0 {
			spool.append(chunk[:count])
		}
		if err != nil {
			spool.finish(err)
			return
		}
	}
}

func (spool *outputSpool) append(data []byte) {
	spool.mu.Lock()
	defer spool.mu.Unlock()

	for len(data) != 0 && !spool.overflow {
		available := maxUnreadOutputBytes - spool.size
		if available == 0 {
			if spool.readerActive && !spool.forceDrain {
				spool.ready.Wait()
				continue
			}
			spool.overflow = true
			break
		}
		count := min(len(data), available)
		if spool.buffer == nil {
			spool.buffer = make([]byte, maxUnreadOutputBytes)
		}
		end := (spool.start + spool.size) % len(spool.buffer)
		first := copy(spool.buffer[end:], data[:count])
		copy(spool.buffer, data[first:count])
		spool.size += count
		data = data[count:]
		spool.ready.Broadcast()
	}
}

func (spool *outputSpool) finish(err error) {
	spool.mu.Lock()
	if !errors.Is(err, io.EOF) {
		spool.readErr = err
	}
	spool.source = nil
	spool.ready.Broadcast()
	spool.mu.Unlock()
}

func (spool *outputSpool) Read(destination []byte) (int, error) {
	if len(destination) == 0 {
		return 0, nil
	}

	spool.mu.Lock()
	defer spool.mu.Unlock()
	spool.readerActive = true
	for spool.size == 0 && spool.source != nil {
		spool.ready.Wait()
	}
	if spool.size != 0 {
		count := min(len(destination), spool.size)
		first := copy(destination[:count], spool.buffer[spool.start:])
		copy(destination[first:count], spool.buffer[:count-first])
		spool.start = (spool.start + count) % len(spool.buffer)
		spool.size -= count
		spool.ready.Broadcast()
		return count, nil
	}
	if spool.overflow {
		return 0, errors.Join(ErrOutputOverflow, spool.readErr)
	}
	if spool.readErr != nil {
		return 0, spool.readErr
	}
	return 0, io.EOF
}

func (spool *outputSpool) forceBoundedDrain() {
	spool.mu.Lock()
	spool.forceDrain = true
	spool.ready.Broadcast()
	spool.mu.Unlock()
}

func (spool *outputSpool) wait() {
	<-spool.done
}
