// Package openai implements the OpenAI-compatible profile's protocol probes.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// OpenAICompatibility reports the capabilities observed at a local endpoint.
type OpenAICompatibility struct {
	Endpoint        string
	Models          []string
	ChatCompletions OpenAIEndpointCapability
	Responses       OpenAIEndpointCapability
	AuthRequired    bool
}

type OpenAIEndpointCapability struct {
	Supported bool
	Streaming bool
}

// ProbeOpenAICompatibility probes the models endpoint and the non-streaming
// chat endpoint. Authentication failures are a useful compatibility result —
// they tell the caller to configure an upstream token instead of saying the
// service is not OpenAI-compatible.
func ProbeOpenAICompatibility(ctx context.Context, baseURL, authToken string) (*OpenAICompatibility, error) {
	base, err := normalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	result := &OpenAICompatibility{Endpoint: base}
	status, body, err := request(ctx, http.MethodGet, joinPath(base, "/v1/models"), authToken, nil)
	if err != nil {
		return nil, fmt.Errorf("probe /v1/models: %w", err)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		result.AuthRequired = true
		return result, nil
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("/v1/models returned status %d", status)
	}
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &models); err != nil {
		return nil, fmt.Errorf("decode /v1/models: %w", err)
	}
	for _, model := range models.Data {
		if model.ID != "" {
			result.Models = append(result.Models, model.ID)
		}
	}
	model := "test"
	if len(result.Models) > 0 {
		model = result.Models[0]
	}
	payload, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     false,
	})
	status, _, err = request(ctx, http.MethodPost, joinPath(base, "/v1/chat/completions"), authToken, payload)
	if err != nil {
		return nil, fmt.Errorf("probe /v1/chat/completions: %w", err)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		result.AuthRequired = true
		return result, nil
	}
	result.ChatCompletions.Supported = status >= 200 && status < 300

	responsesPayload, _ := json.Marshal(map[string]any{
		"model": model, "input": "ping", "stream": false,
	})
	status, _, err = request(ctx, http.MethodPost, joinPath(base, "/v1/responses"), authToken, responsesPayload)
	if err != nil {
		return nil, fmt.Errorf("probe /v1/responses: %w", err)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		result.AuthRequired = true
		return result, nil
	}
	result.Responses.Supported = status >= 200 && status < 300
	return result, nil
}

func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("OpenAI endpoint must be an absolute URL: %q", raw)
	}
	return raw, nil
}

func joinPath(base, path string) string {
	base = strings.TrimRight(base, "/")
	path = "/" + strings.TrimLeft(path, "/")
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(path, "/v1/") {
		path = strings.TrimPrefix(path, "/v1")
	}
	return base + path
}

func request(ctx context.Context, method, endpoint, authToken string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}
