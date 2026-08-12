package codex

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransportRoutesConcurrentResponsesByID(t *testing.T) {
	serverDone := make(chan struct{})
	var joined atomic.Bool
	start := func(context.Context, string) (*process, error) {
		clientReader, serverWriter := io.Pipe()
		serverReader, clientWriter := io.Pipe()
		go func() {
			defer close(serverDone)
			defer func() { _ = serverReader.Close() }()
			defer func() { _ = serverWriter.Close() }()
			decoder := json.NewDecoder(serverReader)
			encoder := json.NewEncoder(serverWriter)

			var initialize fakeRequest
			if decoder.Decode(&initialize) != nil {
				return
			}
			if encoder.Encode(map[string]any{"id": *initialize.ID, "result": map[string]any{}}) != nil {
				return
			}
			var initialized fakeRequest
			if decoder.Decode(&initialized) != nil {
				return
			}
			requests := make([]fakeRequest, 2)
			for index := range requests {
				if decoder.Decode(&requests[index]) != nil {
					return
				}
			}
			for index := len(requests) - 1; index >= 0; index-- {
				request := requests[index]
				if encoder.Encode(map[string]any{
					"id": *request.ID, "result": map[string]string{"value": request.Method},
				}) != nil {
					return
				}
			}
			var trailing any
			_ = decoder.Decode(&trailing)
		}()

		return &process{
			stdin: clientWriter, stdout: clientReader,
			wait: func() error {
				<-serverDone
				joined.Store(true)
				return nil
			},
		}, nil
	}
	client, err := startTransport(t.Context(), "codex-test", start)
	if err != nil {
		t.Fatalf("startTransport() error = %v", err)
	}

	type response struct {
		Value string `json:"value"`
	}
	results := make(chan response, 2)
	errors := make(chan error, 2)
	for _, method := range []string{"first", "second"} {
		go func() {
			var result response
			errors <- client.request(t.Context(), method, map[string]any{}, &result)
			results <- result
		}()
	}
	seen := make(map[string]bool)
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatalf("request() error = %v", err)
		}
		seen[(<-results).Value] = true
	}
	if !seen["first"] || !seen["second"] {
		t.Fatalf("routed responses = %#v", seen)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !joined.Load() {
		t.Fatal("Close() returned before joining the App Server process")
	}
}

func TestTransportLifetimeOutlivesStartupContext(t *testing.T) {
	trace := &fakeTrace{}
	baseStart := newFakeAppServer(t, trace, func(request fakeRequest) fakeReply {
		return successfulReply(t, request, validAnalysisJSON)
	})
	processCanceled := make(chan struct{})
	start := func(ctx context.Context, command string) (*process, error) {
		proc, err := baseStart(ctx, command)
		if err != nil {
			return nil, err
		}
		go func() {
			<-ctx.Done()
			close(processCanceled)
		}()

		return proc, nil
	}
	startupCtx, cancelStartup := context.WithCancel(t.Context())
	client, err := startTransport(startupCtx, "codex-test", start)
	if err != nil {
		t.Fatalf("startTransport() error = %v", err)
	}
	cancelStartup()
	select {
	case <-processCanceled:
		t.Fatal("startup context cancellation stopped the reusable process")
	case <-time.After(10 * time.Millisecond):
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case <-processCanceled:
	case <-time.After(time.Second):
		t.Fatal("Close() did not cancel the reusable process")
	}
}

func TestTransportRejectsOversizedRequest(t *testing.T) {
	client := &transport{done: make(chan struct{})}
	err := client.write(map[string]string{"text": strings.Repeat("x", maxMessageBytes)})
	if err == nil || !strings.Contains(err.Error(), "message limit") {
		t.Fatalf("write() error = %v", err)
	}
}

func TestEventCollectorFailsWhenBoundedQueueFills(t *testing.T) {
	collector := newEventCollector()
	for range collectorChannelDepth + 1 {
		collector.deliver(wireMessage{Method: "event"})
	}
	select {
	case err := <-collector.failed:
		if err == nil {
			t.Fatal("collector failure = nil")
		}
	default:
		t.Fatal("collector did not report queue overflow")
	}
}
