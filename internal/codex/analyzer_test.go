package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/catalog"
	"github.com/dfcut8/assets-library-manager/internal/importer"
)

const validAnalysisJSON = `{
  "title":"Forest Knight",
  "description":"A knight standing in a forest clearing.",
  "primary_type":"character",
  "layout":{"kind":"single","columns":null,"rows":null,"cell_width":null,"cell_height":null,"frame_count":null,"animation_label":null},
  "style":"Painterly",
  "pixel_art":false,
  "confidence":0.91,
  "facets":{"subject":["forest-knight"],"theme":["fantasy"],"material":[],"viewpoint":["front-view"],"composition":[],"palette":["green-brown"]}
}`

func TestAnalyzer_AnalyzeUsesRestrictedStructuredTurn(t *testing.T) {
	trace := &fakeTrace{}
	server := newFakeAppServer(t, trace, func(request fakeRequest) fakeReply {
		return successfulReply(t, request, validAnalysisJSON)
	})
	recorder := &recordingAttempts{}
	analyzer, err := startAnalyzer(t.Context(), testAnalyzerConfig(), recorder, server)
	if err != nil {
		t.Fatalf("startAnalyzer() error = %v", err)
	}
	defer closeAnalyzer(t, analyzer)
	input := testImageInput(t)

	result, provenance, err := analyzer.Analyze(t.Context(), input)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if result.Title != "Forest Knight" || result.PrimaryType != catalog.PrimaryTypeCharacter ||
		result.Layout.Kind != catalog.LayoutKindSingle || len(result.Tags) != 4 {
		t.Fatalf("Analyze() result = %#v", result)
	}
	if provenance.Run.Outcome != "accepted" || provenance.Run.NormalizedResultJSON == "" ||
		provenance.Run.RequestID != "thread-1/turn-1" ||
		provenance.Run.AttemptNumber != 1 {
		t.Fatalf("Analyze() provenance = %#v", provenance)
	}
	if provenance.TokenUsage.InputTokens != 10 || provenance.TokenUsage.OutputTokens != 20 ||
		provenance.TokenUsage.TotalTokens != 30 {
		t.Fatalf("Analyze() token usage = %#v", provenance.TokenUsage)
	}
	if len(recorder.runs()) != 0 {
		t.Fatal("accepted attempt was recorded outside the atomic asset commit")
	}
	thread := trace.first(t, "thread/start")
	var threadParams struct {
		Ephemeral      bool   `json:"ephemeral"`
		ApprovalPolicy string `json:"approvalPolicy"`
		Sandbox        string `json:"sandbox"`
	}
	if err := json.Unmarshal(thread.Params, &threadParams); err != nil {
		t.Fatalf("decoding thread params: %v", err)
	}
	if !threadParams.Ephemeral || threadParams.ApprovalPolicy != "never" || threadParams.Sandbox != "read-only" {
		t.Fatalf("thread params = %#v", threadParams)
	}
	var rawThreadParams map[string]any
	if err := json.Unmarshal(thread.Params, &rawThreadParams); err != nil {
		t.Fatalf("decoding raw thread params: %v", err)
	}
	if rawThreadParams["cwd"] != analyzer.config.WorkingDirectory ||
		rawThreadParams["cwd"] == input.ScratchDirectory {
		t.Fatalf("thread cwd = %v", rawThreadParams["cwd"])
	}

	turn := trace.first(t, "turn/start")
	var params struct {
		ApprovalPolicy string `json:"approvalPolicy"`
		SandboxPolicy  struct {
			Type          string `json:"type"`
			NetworkAccess bool   `json:"networkAccess"`
		} `json:"sandboxPolicy"`
		OutputSchema map[string]any `json:"outputSchema"`
		Input        []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Path string `json:"path"`
		} `json:"input"`
	}
	if err := json.Unmarshal(turn.Params, &params); err != nil {
		t.Fatalf("decoding turn params: %v", err)
	}
	if params.ApprovalPolicy != "never" || params.SandboxPolicy.Type != "readOnly" ||
		params.SandboxPolicy.NetworkAccess {
		t.Fatalf("turn sandbox params = %#v", params)
	}
	var rawParams map[string]any
	if err := json.Unmarshal(turn.Params, &rawParams); err != nil {
		t.Fatalf("decoding raw turn params: %v", err)
	}
	sandboxPolicy := rawParams["sandboxPolicy"].(map[string]any)
	if _, ok := sandboxPolicy["access"]; ok {
		t.Fatalf("turn sandbox policy contains removed access field: %#v", sandboxPolicy)
	}
	if params.OutputSchema["additionalProperties"] != false || len(params.Input) != 2 ||
		params.Input[1].Type != "localImage" || params.Input[1].Path == "" {
		t.Fatalf("turn input/schema params = %#v", params)
	}
	if rawParams["cwd"] != analyzer.config.WorkingDirectory ||
		rawParams["cwd"] == input.ScratchDirectory {
		t.Fatalf("turn cwd = %v", rawParams["cwd"])
	}
	if strings.Contains(params.Input[0].Text, params.Input[1].Path) {
		t.Fatal("prompt contains the local image path")
	}
	if trace.count("thread/delete") != 0 {
		t.Fatalf("ephemeral thread/delete count = %d, want 0", trace.count("thread/delete"))
	}
	initialize := trace.first(t, "initialize")
	var initializeParams struct {
		Capabilities struct {
			OptOutNotificationMethods []string `json:"optOutNotificationMethods"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(initialize.Params, &initializeParams); err != nil {
		t.Fatalf("decoding initialize params: %v", err)
	}
	for _, method := range initializeParams.Capabilities.OptOutNotificationMethods {
		if method == "thread/tokenUsage/updated" {
			t.Fatal("initialize opts out of token usage notifications")
		}
	}
}

func TestAnalyzer_AnalyzeRetriesInvalidResponseAndPersistsFailure(t *testing.T) {
	trace := &fakeTrace{}
	turns := 0
	server := newFakeAppServer(t, trace, func(request fakeRequest) fakeReply {
		if request.Method == "turn/start" {
			turns++
			if turns == 1 {
				return successfulReply(t, request, `{"title":`)
			}
		}

		return successfulReply(t, request, validAnalysisJSON)
	})
	recorder := &recordingAttempts{}
	analyzer, err := startAnalyzer(t.Context(), testAnalyzerConfig(), recorder, server)
	if err != nil {
		t.Fatalf("startAnalyzer() error = %v", err)
	}
	analyzer.random = func() float64 { return 0 }
	defer closeAnalyzer(t, analyzer)

	_, provenance, err := analyzer.Analyze(t.Context(), testImageInput(t))
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if provenance.Run.AttemptNumber != 2 {
		t.Fatalf("accepted attempt = %d, want 2", provenance.Run.AttemptNumber)
	}
	if provenance.TokenUsage.TotalTokens != 60 {
		t.Fatalf("retry token usage = %#v, want 60 total tokens", provenance.TokenUsage)
	}
	runs := recorder.runs()
	if len(runs) != 1 || runs[0].Outcome != "invalid-response" ||
		runs[0].NormalizedResultJSON != "" || strings.Contains(runs[0].ErrorMessage, `{"title"`) {
		t.Fatalf("recorded runs = %#v", runs)
	}
	if trace.count("thread/start") != 2 || trace.count("thread/delete") != 0 {
		t.Fatalf("thread lifecycle counts = start %d delete %d", trace.count("thread/start"), trace.count("thread/delete"))
	}
}

func TestClassifyTransportErrorIncludesRPCMethodAndMessage(t *testing.T) {
	err := classifyTransportError(&rpcRequestError{
		Method: "turn/start",
		Cause:  &rpcError{Code: -32600, Message: "Invalid request"},
	})
	if !strings.Contains(err.Message, "method turn/start") ||
		!strings.Contains(err.Message, "message Invalid request") {
		t.Fatalf("diagnostic = %q", err.Message)
	}
}

func TestAnalyzer_AnalyzeDoesNotRetryRefusal(t *testing.T) {
	trace := &fakeTrace{}
	server := newFakeAppServer(t, trace, func(request fakeRequest) fakeReply {
		if request.Method == "turn/start" {
			return turnReply("failed", "safety_refusal", "provider details")
		}

		return successfulReply(t, request, validAnalysisJSON)
	})
	recorder := &recordingAttempts{}
	analyzer, err := startAnalyzer(t.Context(), testAnalyzerConfig(), recorder, server)
	if err != nil {
		t.Fatalf("startAnalyzer() error = %v", err)
	}
	defer closeAnalyzer(t, analyzer)

	_, provenance, err := analyzer.Analyze(t.Context(), testImageInput(t))
	var classified *AnalysisError
	if !errors.As(err, &classified) || classified.Kind != ErrorRefused || classified.Retryable {
		t.Fatalf("Analyze() error = %#v", err)
	}
	runs := recorder.runs()
	if len(runs) != 1 || runs[0].Outcome != "refused" || strings.Contains(runs[0].ErrorMessage, "provider details") {
		t.Fatalf("recorded runs = %#v", runs)
	}
	if trace.count("turn/start") != 1 {
		t.Fatalf("turn/start count = %d, want 1", trace.count("turn/start"))
	}
	if provenance.TokenUsage.TotalTokens != 30 {
		t.Fatalf("failed analysis token usage = %#v, want 30 total tokens", provenance.TokenUsage)
	}
}

func TestAnalyzer_AnalyzeTimesOutInterruptsAndRecordsCancellation(t *testing.T) {
	trace := &fakeTrace{}
	server := newFakeAppServer(t, trace, func(request fakeRequest) fakeReply {
		if request.Method == "turn/start" {
			return fakeReply{Result: map[string]any{"turn": map[string]string{"id": "turn-1"}}}
		}

		return successfulReply(t, request, validAnalysisJSON)
	})
	recorder := &recordingAttempts{}
	config := testAnalyzerConfig()
	config.TurnTimeout = 20 * time.Millisecond
	analyzer, err := startAnalyzer(t.Context(), config, recorder, server)
	if err != nil {
		t.Fatalf("startAnalyzer() error = %v", err)
	}
	defer closeAnalyzer(t, analyzer)

	_, _, err = analyzer.Analyze(t.Context(), testImageInput(t))
	var classified *AnalysisError
	if !errors.As(err, &classified) || classified.Kind != ErrorDeadline || classified.Retryable {
		t.Fatalf("Analyze() error = %#v", err)
	}
	runs := recorder.runs()
	if len(runs) != 1 || runs[0].Outcome != "canceled" {
		t.Fatalf("recorded runs = %#v", runs)
	}
	if trace.count("turn/interrupt") != 1 || trace.count("thread/delete") != 0 {
		t.Fatalf("cleanup counts = interrupt %d delete %d", trace.count("turn/interrupt"), trace.count("thread/delete"))
	}
}

func TestAnalyzer_AnalyzeHonorsCallerCancellationBeforeWork(t *testing.T) {
	trace := &fakeTrace{}
	server := newFakeAppServer(t, trace, func(request fakeRequest) fakeReply {
		return successfulReply(t, request, validAnalysisJSON)
	})
	analyzer, err := startAnalyzer(t.Context(), testAnalyzerConfig(), &recordingAttempts{}, server)
	if err != nil {
		t.Fatalf("startAnalyzer() error = %v", err)
	}
	defer closeAnalyzer(t, analyzer)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err = analyzer.Analyze(ctx, testImageInput(t))
	var classified *AnalysisError
	if !errors.As(err, &classified) || classified.Kind != ErrorCanceled || classified.Retryable ||
		!errors.Is(err, context.Canceled) {
		t.Fatalf("Analyze() error = %#v", err)
	}
	if trace.count("turn/start") != 0 {
		t.Fatal("analysis turn started after caller cancellation")
	}
}

func TestStartAnalyzer_RejectsIncompatiblePreflight(t *testing.T) {
	tests := []struct {
		name   string
		method string
		reply  fakeReply
		kind   ErrorKind
	}{
		{
			name: "signed out", method: "account/read",
			reply: fakeReply{Result: map[string]any{"account": nil}}, kind: ErrorAuthentication,
		},
		{
			name: "model missing", method: "model/list",
			reply: fakeReply{Result: map[string]any{"data": []any{}}}, kind: ErrorConfiguration,
		},
		{
			name: "image unsupported", method: "model/list",
			reply: fakeReply{Result: modelResult([]string{"text"}, "medium")}, kind: ErrorConfiguration,
		},
		{
			name: "effort unsupported", method: "model/list",
			reply: fakeReply{Result: modelResult([]string{"text", "image"}, "low")}, kind: ErrorConfiguration,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trace := &fakeTrace{}
			server := newFakeAppServer(t, trace, func(request fakeRequest) fakeReply {
				if request.Method == tt.method {
					return tt.reply
				}

				return successfulReply(t, request, validAnalysisJSON)
			})
			_, err := startAnalyzer(t.Context(), testAnalyzerConfig(), &recordingAttempts{}, server)
			var classified *AnalysisError
			if !errors.As(err, &classified) || classified.Kind != tt.kind {
				t.Fatalf("startAnalyzer() error = %#v, want kind %q", err, tt.kind)
			}
		})
	}
}

func TestStartAnalyzer_RejectsInvalidWorkingDirectoryBeforeStartingProcess(t *testing.T) {
	config := testAnalyzerConfig()
	config.WorkingDirectory = filepath.Join(t.TempDir(), "missing")
	started := false
	_, err := startAnalyzer(
		t.Context(), config, &recordingAttempts{},
		func(context.Context, string) (*process, error) {
			started = true

			return nil, errors.New("unexpected process start")
		},
	)
	var classified *AnalysisError
	if !errors.As(err, &classified) || classified.Kind != ErrorConfiguration {
		t.Fatalf("startAnalyzer() error = %#v", err)
	}
	if started {
		t.Fatal("Codex process started with an invalid analysis workspace")
	}
}

func TestAnalyzer_AnalyzeRespectsRateLimitPreflight(t *testing.T) {
	trace := &fakeTrace{}
	server := newFakeAppServer(t, trace, func(request fakeRequest) fakeReply {
		if request.Method == "account/rateLimits/read" {
			return fakeReply{Result: map[string]any{"rateLimits": map[string]any{
				"rateLimitReachedType": "primary",
				"primary":              map[string]any{"usedPercent": 100},
			}}}
		}

		return successfulReply(t, request, validAnalysisJSON)
	})
	config := testAnalyzerConfig()
	config.InitialRetryDelay = time.Nanosecond
	analyzer, err := startAnalyzer(t.Context(), config, &recordingAttempts{}, server)
	if err != nil {
		t.Fatalf("startAnalyzer() error = %v", err)
	}
	analyzer.random = func() float64 { return 0 }
	defer closeAnalyzer(t, analyzer)

	_, _, err = analyzer.Analyze(t.Context(), testImageInput(t))
	var classified *AnalysisError
	if !errors.As(err, &classified) || classified.Kind != ErrorRetryable ||
		!errors.Is(err, errRateLimited) {
		t.Fatalf("Analyze() error = %#v", err)
	}
	if trace.count("turn/start") != 0 {
		t.Fatal("analysis turn started while rate limited")
	}
}

func TestValidateImageInputRejectsUnexpectedScratchContent(t *testing.T) {
	input := testImageInput(t)
	if err := os.WriteFile(filepath.Join(input.ScratchDirectory, "unrelated.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := validateImageInput(input)
	var classified *AnalysisError
	if !errors.As(err, &classified) || classified.Kind != ErrorPermanent {
		t.Fatalf("validateImageInput() error = %#v", err)
	}
}

type recordedAttempt struct {
	itemID  importer.ID
	assetID importer.ID
	run     importer.AIRun
}

type recordingAttempts struct {
	mu       sync.Mutex
	attempts []recordedAttempt
}

func (recorder *recordingAttempts) RecordAIRun(
	_ context.Context,
	itemID importer.ID,
	assetID importer.ID,
	run importer.AIRun,
) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.attempts = append(recorder.attempts, recordedAttempt{itemID: itemID, assetID: assetID, run: run})

	return nil
}

func (recorder *recordingAttempts) runs() []importer.AIRun {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	runs := make([]importer.AIRun, 0, len(recorder.attempts))
	for _, attempt := range recorder.attempts {
		runs = append(runs, attempt.run)
	}

	return runs
}

type fakeRequest struct {
	Method string          `json:"method"`
	ID     *int64          `json:"id"`
	Params json.RawMessage `json:"params"`
}

type fakeReply struct {
	Result any
	Error  *rpcError
	Events []any
}

type fakeTrace struct {
	mu       sync.Mutex
	requests []fakeRequest
}

func (trace *fakeTrace) add(request fakeRequest) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.requests = append(trace.requests, request)
}

func (trace *fakeTrace) count(method string) int {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	count := 0
	for _, request := range trace.requests {
		if request.Method == method {
			count++
		}
	}

	return count
}

func (trace *fakeTrace) first(t *testing.T, method string) fakeRequest {
	t.Helper()
	trace.mu.Lock()
	defer trace.mu.Unlock()
	for _, request := range trace.requests {
		if request.Method == method {
			return request
		}
	}
	t.Fatalf("no %s request", method)

	return fakeRequest{}
}

func newFakeAppServer(
	t *testing.T,
	trace *fakeTrace,
	handler func(fakeRequest) fakeReply,
) startFunc {
	t.Helper()

	return func(context.Context, string) (*process, error) {
		clientReader, serverWriter := io.Pipe()
		serverReader, clientWriter := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() { _ = serverReader.Close() }()
			defer func() { _ = serverWriter.Close() }()
			decoder := json.NewDecoder(serverReader)
			encoder := json.NewEncoder(serverWriter)
			for {
				var request fakeRequest
				if err := decoder.Decode(&request); err != nil {
					return
				}
				trace.add(request)
				if request.ID == nil {
					continue
				}
				reply := handler(request)
				response := map[string]any{"id": *request.ID, "result": reply.Result}
				if reply.Error != nil {
					response = map[string]any{"id": *request.ID, "error": reply.Error}
				}
				if err := encoder.Encode(response); err != nil {
					return
				}
				for _, event := range reply.Events {
					if err := encoder.Encode(event); err != nil {
						return
					}
				}
			}
		}()

		return &process{
			stdin: clientWriter, stdout: clientReader,
			wait: func() error {
				<-done
				return nil
			},
		}, nil
	}
}

func successfulReply(t *testing.T, request fakeRequest, analysis string) fakeReply {
	t.Helper()
	switch request.Method {
	case "initialize":
		return fakeReply{Result: map[string]any{"serverInfo": map[string]string{"name": "fake"}}}
	case "account/read":
		return fakeReply{Result: map[string]any{"account": map[string]any{"type": "chatgpt", "planType": "plus"}}}
	case "account/rateLimits/read":
		return fakeReply{Result: map[string]any{"rateLimits": map[string]any{
			"primary": map[string]any{"usedPercent": 1},
		}}}
	case "model/list":
		return fakeReply{Result: modelResult([]string{"text", "image"}, "medium")}
	case "thread/start":
		return fakeReply{Result: map[string]any{"thread": map[string]string{"id": "thread-1"}}}
	case "turn/start":
		return turnReply("completed", "", analysis)
	case "turn/interrupt", "thread/delete":
		return fakeReply{Result: map[string]any{}}
	default:
		t.Errorf("unexpected fake App Server method %q", request.Method)
		return fakeReply{Error: &rpcError{Code: -32601, Message: "unexpected test method"}}
	}
}

func turnReply(status, code, text string) fakeReply {
	item := map[string]any{
		"method": "item/completed",
		"params": map[string]any{
			"threadId": "thread-1", "turnId": "turn-1",
			"item": map[string]any{"type": "agentMessage", "text": text},
		},
	}
	turn := map[string]any{
		"method": "turn/completed",
		"params": map[string]any{
			"threadId": "thread-1",
			"turn": map[string]any{
				"id": "turn-1", "status": status,
			},
		},
	}
	usage := map[string]any{
		"method": "thread/tokenUsage/updated",
		"params": map[string]any{
			"threadId": "thread-1", "turnId": "turn-1",
			"tokenUsage": map[string]any{
				"total": map[string]any{
					"inputTokens": 10, "cachedInputTokens": 2, "cacheWriteInputTokens": 1,
					"outputTokens": 20, "reasoningOutputTokens": 5, "totalTokens": 30,
				},
			},
		},
	}
	if status != "completed" {
		turnParams := turn["params"].(map[string]any)
		turnData := turnParams["turn"].(map[string]any)
		turnData["error"] = map[string]string{"code": code, "message": text}
	}

	return fakeReply{
		Result: map[string]any{"turn": map[string]string{"id": "turn-1"}},
		Events: []any{item, usage, turn},
	}
}

func modelResult(modalities []string, effort string) map[string]any {
	return map[string]any{"data": []any{map[string]any{
		"id": "gpt-test", "inputModalities": modalities,
		"supportedReasoningEfforts": []any{map[string]string{"reasoningEffort": effort}},
	}}}
}

func testAnalyzerConfig() AnalyzerConfig {
	return AnalyzerConfig{
		Command: "codex-test", Model: "gpt-test", ReasoningEffort: "medium",
		WorkingDirectory: os.TempDir(),
		TurnTimeout:      time.Second, MaxAttempts: 2, InitialRetryDelay: time.Nanosecond,
	}
}

func testImageInput(t *testing.T) ImageInput {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "analysis.png")
	if err := os.WriteFile(path, []byte("bounded image bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	itemID, err := importer.NewID()
	if err != nil {
		t.Fatal(err)
	}

	return ImageInput{
		ItemID: itemID, Path: path, ScratchDirectory: directory,
		DisplayWidth: 64, DisplayHeight: 64,
	}
}

func closeAnalyzer(t *testing.T, analyzer *Analyzer) {
	t.Helper()
	if err := analyzer.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
}
