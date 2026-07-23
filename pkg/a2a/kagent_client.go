package a2a

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"strings"
	"time"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"
)

// partKind is the discriminator key in kagent's spec-lowercase part encoding.
const partKind = "kind"

// ErrPayloadTooLarge is returned when kagent rejects a turn because its request
// body exceeds the agent's configured A2A_MAX_CONTENT_LENGTH (HTTP 413). The
// body may be over the cap because of attachments or a large accumulated
// history; the cause is not distinguished here. Channels match it with errors.Is
// to render an actionable "too large" notice instead of the generic turn-failed
// message.
var ErrPayloadTooLarge = errors.New("a2a: request payload too large")

// agentRefKey is the context key for the target agentRef.
type agentRefKey struct{}

// WithAgentRef stores agentRef in ctx.
func WithAgentRef(ctx context.Context, agentRef string) context.Context {
	return context.WithValue(ctx, agentRefKey{}, agentRef)
}

// AgentRefFromContext returns the agentRef stored by WithAgentRef, or empty string.
func AgentRefFromContext(ctx context.Context) string {
	ref, _ := ctx.Value(agentRefKey{}).(string)
	return ref
}

// defaultStreamClient carries the "message/stream" SSE responses. It has no
// overall timeout: Client.Timeout covers the entire body read, so any value
// caps turn duration and severs the stream mid-turn, leaving the agent session
// with a tool call that never gets its result. Connection establishment is
// still bounded (dial, TLS, response headers); the stream's lifetime is the
// caller's ctx.
var defaultStreamClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	},
}

// A2AClient forwards channel turns to a kagent A2A orchestrator endpoint using
// direct HTTP with the "message/stream" JSON-RPC method.
type A2AClient struct {
	// HTTPClient is the HTTP client used for requests. Nil uses a default with
	// no overall timeout: the response is a long-lived SSE stream whose lifetime
	// is bounded by ctx (an http.Client.Timeout would cut a turn mid-stream).
	HTTPClient *http.Client
	// BaseURL is the kagent A2A base URL, e.g. http://kagent-controller.kagent.svc.cluster.local:8083/api/a2a/kagent
	BaseURL string
	// DefaultAgent is the agentRef used when the context carries none.
	DefaultAgent string
	// TokenSource yields the Bearer token sent as Authorization on each request.
	// Nil sends no Authorization header.
	TokenSource TokenSource
}

// Execute sends the inbound message via A2A streaming ("message/stream") and yields events.
// The target agent is read from ctx via WithAgentRef, falling back to DefaultAgent.
func (k *A2AClient) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2apkg.Event, error] {
	return func(yield func(a2apkg.Event, error) bool) {
		agentRef := AgentRefFromContext(ctx)
		if agentRef == "" {
			agentRef = k.DefaultAgent
		}

		endpoint := strings.TrimRight(k.BaseURL, "/") + "/" + agentRef + "/a2a"

		var token string
		if k.TokenSource != nil {
			t, err := k.TokenSource.Token(ctx)
			if err != nil {
				yield(nil, err)
				return
			}
			token = t
		}

		reqBody, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"method":  "message/stream",
			"params":  buildKagentParams(execCtx),
			"id":      uuid.NewString(),
		})
		if err != nil {
			yield(nil, fmt.Errorf("marshal a2a request: %w", err))
			return
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(reqBody))
		if err != nil {
			yield(nil, fmt.Errorf("create a2a request: %w", err))
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		if token != "" {
			httpReq.Header.Set("Authorization", "Bearer "+token)
		}

		httpClient := k.HTTPClient
		if httpClient == nil {
			httpClient = defaultStreamClient
		}

		resp, err := httpClient.Do(httpReq)
		if err != nil {
			yield(nil, fmt.Errorf("a2a request: %w", err))
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			if resp.StatusCode == http.StatusRequestEntityTooLarge {
				yield(nil, fmt.Errorf("%w: %s", ErrPayloadTooLarge, resp.Status))
				return
			}
			yield(nil, fmt.Errorf("unexpected HTTP status: %s", resp.Status))
			return
		}

		for event, err := range parseKagentSSEStream(resp.Body) {
			if !yield(event, err) {
				return
			}
		}
	}
}

// kagentStreamResult holds the "result" field of a kagent JSON-RPC SSE response.
// kagent uses a "kind" discriminator rather than the library's wrapper-field convention.
type kagentStreamResult struct {
	Kind      string           `json:"kind"`
	ContextID string           `json:"contextId,omitempty"`
	TaskID    string           `json:"taskId,omitempty"`
	Final     bool             `json:"final,omitempty"`
	Status    *kagentStatus    `json:"status,omitempty"`
	Artifact  *a2apkg.Artifact `json:"artifact,omitempty"`
	LastChunk bool             `json:"lastChunk,omitempty"`
	Metadata  map[string]any   `json:"metadata,omitempty"`
}

// kagentStatus holds a task status using the kagent/A2A-spec lowercase state strings.
// The a2a-go library constants use TASK_STATE_* uppercase; this type bridges that gap.
type kagentStatus struct {
	State   string          `json:"state"`
	Message *a2apkg.Message `json:"message,omitempty"`
}

// toA2AStatus converts the kagent status to the a2a-go library TaskStatus.
func (s *kagentStatus) toA2AStatus() a2apkg.TaskStatus {
	return a2apkg.TaskStatus{
		State:   mapKagentState(s.State),
		Message: s.Message,
	}
}

// mapKagentState converts kagent's lowercase spec state strings to a2a-go library constants.
func mapKagentState(s string) a2apkg.TaskState {
	switch s {
	case "completed":
		return a2apkg.TaskStateCompleted
	case "failed":
		return a2apkg.TaskStateFailed
	case "canceled":
		return a2apkg.TaskStateCanceled
	case "submitted":
		return a2apkg.TaskStateSubmitted
	case "working":
		return a2apkg.TaskStateWorking
	case "input-required":
		return a2apkg.TaskStateInputRequired
	case "auth-required":
		return a2apkg.TaskStateAuthRequired
	case "rejected":
		return a2apkg.TaskStateRejected
	default:
		return a2apkg.TaskState(s)
	}
}

// buildKagentParams builds the message/stream params in the spec-lowercase format that kagent
// expects. The a2a-go v2.3.1 library uses proto-style uppercase constants (ROLE_USER,
// TASK_STATE_COMPLETED) which kagent rejects with 400; this function emits the spec strings.
func buildKagentParams(execCtx *a2asrv.ExecutorContext) map[string]any {
	parts := make([]map[string]any, 0, len(execCtx.Message.Parts))
	for _, p := range execCtx.Message.Parts {
		if p == nil {
			continue
		}
		if text := p.Text(); text != "" {
			parts = append(parts, map[string]any{partKind: "text", "text": text})
			continue
		}
		// Structured data parts carry HITL decisions (decision_type,
		// ask_user_answers). kagent resolves the paused confirmation only from
		// a DataPart, so these must survive serialization.
		if d := p.Data(); d != nil {
			parts = append(parts, map[string]any{partKind: "data", "data": d})
			continue
		}
		// Raw parts are file attachments; kagent expects the spec FilePart shape
		// with base64-encoded bytes.
		if raw := p.Raw(); raw != nil {
			parts = append(parts, map[string]any{
				partKind: "file",
				"file": map[string]any{
					"name":     p.Filename,
					"mimeType": p.MediaType,
					"bytes":    base64.StdEncoding.EncodeToString(raw),
				},
			})
		}
	}

	role := "user"
	if execCtx.Message.Role == a2apkg.MessageRoleAgent {
		role = "agent"
	}

	msg := map[string]any{
		"messageId": execCtx.Message.ID,
		"role":      role,
		"parts":     parts,
	}
	if execCtx.ContextID != "" {
		msg["contextId"] = execCtx.ContextID
	}

	params := map[string]any{"message": msg}
	if execCtx.TaskID != "" {
		params["taskId"] = string(execCtx.TaskID)
	}
	if execCtx.Metadata != nil {
		params["metadata"] = execCtx.Metadata
	}
	return params
}

// parseKagentSSEStream reads an SSE body from kagent's A2A endpoint and yields events.
// The stream contains JSON-RPC responses with a `result` field using kagent's "kind" discriminator.
func parseKagentSSEStream(body io.Reader) iter.Seq2[a2apkg.Event, error] {
	return func(yield func(a2apkg.Event, error) bool) {
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimPrefix(line, "data:")
			if len(data) > 0 && data[0] == ' ' {
				data = data[1:]
			}

			var wrapper struct {
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(data), &wrapper); err != nil {
				if !yield(nil, fmt.Errorf("parse sse data: %w", err)) {
					return
				}
				continue
			}

			if wrapper.Error != nil {
				yield(nil, fmt.Errorf("a2a error %d: %s", wrapper.Error.Code, wrapper.Error.Message))
				return
			}

			if wrapper.Result == nil {
				continue
			}

			var result kagentStreamResult
			if err := json.Unmarshal(wrapper.Result, &result); err != nil {
				if !yield(nil, fmt.Errorf("parse a2a result: %w", err)) {
					return
				}
				continue
			}

			var event a2apkg.Event
			switch result.Kind {
			case "status-update":
				ev := &a2apkg.TaskStatusUpdateEvent{
					ContextID: result.ContextID,
					TaskID:    a2apkg.TaskID(result.TaskID),
					Metadata:  result.Metadata,
				}
				if result.Status != nil {
					ev.Status = result.Status.toA2AStatus()
				}
				event = ev
			case "artifact-update":
				event = &a2apkg.TaskArtifactUpdateEvent{
					ContextID: result.ContextID,
					TaskID:    a2apkg.TaskID(result.TaskID),
					Artifact:  result.Artifact,
					LastChunk: result.LastChunk,
					Metadata:  result.Metadata,
				}
			default:
				continue
			}

			if !yield(event, nil) {
				return
			}

			if result.Final {
				return
			}
		}

		if err := scanner.Err(); err != nil {
			yield(nil, fmt.Errorf("sse stream: %w", err))
		}
	}
}
