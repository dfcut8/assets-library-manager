package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
)

const (
	maxMessageBytes       = 1 << 20
	maxPendingRequests    = 128
	maxCollectedMessages  = 512
	collectorChannelDepth = 32
)

type startFunc func(context.Context, string) (*process, error)

type process struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	wait   func() error
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("app server error %d", e.Code)
}

type wireMessage struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type eventCollector struct {
	messages chan wireMessage
	failed   chan error
	once     sync.Once
}

func newEventCollector() *eventCollector {
	return &eventCollector{
		messages: make(chan wireMessage, collectorChannelDepth),
		failed:   make(chan error, 1),
	}
}

func (collector *eventCollector) deliver(message wireMessage) {
	select {
	case collector.messages <- message:
	default:
		collector.fail(errors.New("app server event collector exceeded its bounded queue"))
	}
}

func (collector *eventCollector) fail(err error) {
	collector.once.Do(func() {
		collector.failed <- err
	})
}

// transport owns one App Server process and its single stdout reader.
type transport struct {
	cancel    context.CancelFunc
	proc      *process
	nextID    atomic.Int64
	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[int64]chan wireMessage
	eventsMu  sync.RWMutex
	events    map[*eventCollector]string
	done      chan struct{}
	readErrMu sync.Mutex
	readErr   error
	closing   atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func startTransport(ctx context.Context, command string, start startFunc) (*transport, error) {
	processCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	proc, err := start(processCtx, command)
	if err != nil {
		cancel()

		return nil, err
	}
	client := &transport{
		cancel: cancel, proc: proc,
		pending: make(map[int64]chan wireMessage),
		events:  make(map[*eventCollector]string),
		done:    make(chan struct{}),
	}
	client.nextID.Store(0)
	go client.readLoop()

	var initialized struct {
		UserAgent string `json:"userAgent"`
	}
	if err := client.request(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{
			"name": "asset_library_manager", "title": "Asset Library Manager", "version": "0.1.0",
		},
		"capabilities": map[string]any{
			"optOutNotificationMethods": []string{
				"item/agentMessage/delta", "item/reasoning/summaryTextDelta",
				"item/reasoning/summaryPartAdded", "item/reasoning/textDelta",
				"thread/tokenUsage/updated",
			},
		},
	}, &initialized); err != nil {
		return nil, errors.Join(fmt.Errorf("initializing app server: %w", err), client.Close())
	}
	if err := client.notify("initialized", map[string]any{}); err != nil {
		return nil, errors.Join(fmt.Errorf("acknowledging app server: %w", err), client.Close())
	}

	return client, nil
}

func (client *transport) request(ctx context.Context, method string, params any, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id := client.nextID.Add(1)
	responseChannel := make(chan wireMessage, 1)
	client.pendingMu.Lock()
	if len(client.pending) >= maxPendingRequests {
		client.pendingMu.Unlock()

		return errors.New("app server pending request limit exceeded")
	}
	client.pending[id] = responseChannel
	client.pendingMu.Unlock()
	defer client.removePending(id)

	if err := client.write(map[string]any{"method": method, "id": id, "params": params}); err != nil {
		return err
	}
	select {
	case message := <-responseChannel:
		if message.Error != nil {
			return message.Error
		}
		if len(message.Result) == 0 {
			return errors.New("app server response has no result")
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(message.Result, result); err != nil {
			return fmt.Errorf("decoding app server response: %w", err)
		}

		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-client.done:
		return client.transportError()
	}
}

func (client *transport) notify(method string, params any) error {
	return client.write(map[string]any{"method": method, "params": params})
}

func (client *transport) write(message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encoding app server message: %w", err)
	}
	if len(data)+1 > maxMessageBytes {
		return errors.New("app server request exceeds message limit")
	}
	data = append(data, '\n')
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	select {
	case <-client.done:
		return client.transportError()
	default:
	}
	for len(data) > 0 {
		written, writeErr := client.proc.stdin.Write(data)
		if writeErr != nil {
			return fmt.Errorf("writing app server message: %w", writeErr)
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}

	return nil
}

func (client *transport) collect(threadID string) (*eventCollector, func()) {
	collector := newEventCollector()
	client.eventsMu.Lock()
	client.events[collector] = threadID
	client.eventsMu.Unlock()

	return collector, func() {
		client.eventsMu.Lock()
		delete(client.events, collector)
		client.eventsMu.Unlock()
	}
}

func (client *transport) readLoop() {
	defer close(client.done)
	scanner := bufio.NewScanner(client.proc.stdout)
	scanner.Buffer(make([]byte, 64<<10), maxMessageBytes)
	consecutiveNotifications := 0
	for scanner.Scan() {
		var message wireMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			client.setReadError(fmt.Errorf("decoding app server message: %w", err))
			return
		}
		if message.ID != nil && message.Method == "" {
			consecutiveNotifications = 0
			client.pendingMu.Lock()
			channel := client.pending[*message.ID]
			client.pendingMu.Unlock()
			if channel != nil {
				channel <- message
			}
			continue
		}
		if message.Method == "" {
			client.setReadError(errors.New("app server message has neither response id nor method"))
			return
		}
		if message.ID != nil {
			client.respondUnsupported(*message.ID)
			continue
		}
		consecutiveNotifications++
		if consecutiveNotifications > maxCollectedMessages {
			client.setReadError(errors.New("app server notification limit exceeded"))
			return
		}
		var eventScope struct {
			ThreadID string `json:"threadId"`
		}
		if len(message.Params) > 0 {
			if err := json.Unmarshal(message.Params, &eventScope); err != nil {
				client.setReadError(fmt.Errorf("decoding app server notification scope: %w", err))
				return
			}
		}
		if eventScope.ThreadID == "" {
			continue
		}
		client.eventsMu.RLock()
		for collector, threadID := range client.events {
			if eventScope.ThreadID == threadID {
				collector.deliver(message)
			}
		}
		client.eventsMu.RUnlock()
	}
	if err := scanner.Err(); err != nil {
		client.setReadError(fmt.Errorf("reading app server message: %w", err))
	} else if client.closing.Load() {
		client.setReadError(errors.New("app server transport closed"))
	} else {
		client.setReadError(io.ErrUnexpectedEOF)
	}
}

func (client *transport) respondUnsupported(id int64) {
	_ = client.write(map[string]any{
		"id":    id,
		"error": map[string]any{"code": -32601, "message": "client does not implement server requests"},
	})
}

func (client *transport) removePending(id int64) {
	client.pendingMu.Lock()
	delete(client.pending, id)
	client.pendingMu.Unlock()
}

func (client *transport) setReadError(err error) {
	client.readErrMu.Lock()
	if client.readErr == nil {
		client.readErr = err
	}
	client.readErrMu.Unlock()
	client.eventsMu.RLock()
	for collector := range client.events {
		collector.fail(err)
	}
	client.eventsMu.RUnlock()
}

func (client *transport) transportError() error {
	client.readErrMu.Lock()
	defer client.readErrMu.Unlock()
	if client.readErr != nil {
		return client.readErr
	}

	return errors.New("app server transport closed")
}

func (client *transport) Close() error {
	client.closeOnce.Do(func() {
		client.closing.Store(true)
		client.cancel()
		stdinErr := closePipe("codex stdin", client.proc.stdin)
		stdoutErr := closePipe("codex stdout", client.proc.stdout)
		waitErr := client.proc.wait()
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) || errors.Is(waitErr, context.Canceled) {
			waitErr = nil
		}
		<-client.done
		client.closeErr = errors.Join(stdinErr, stdoutErr, waitErr)
	})

	return client.closeErr
}

func startCommand(ctx context.Context, command string) (*process, error) {
	path, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("locating codex command %q: %w", command, err)
	}
	cmd := exec.CommandContext(ctx, path, "app-server", "--listen", "stdio://")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("opening codex stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("opening codex stdout: %w", err), closePipe("codex stdin", stdin))
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("starting codex app server: %w", err),
			closePipe("codex stdin", stdin), closePipe("codex stdout", stdout),
		)
	}

	return &process{stdin: stdin, stdout: stdout, wait: cmd.Wait}, nil
}

func closePipe(name string, closer io.Closer) error {
	if closer == nil {
		return nil
	}
	if err := closer.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		return fmt.Errorf("closing %s: %w", name, err)
	}

	return nil
}
