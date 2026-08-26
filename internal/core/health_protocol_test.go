package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Audit R5: MCP and OpenAI service health must prove PROTOCOL validity, not
// merely HTTP reachability. A 200 carrying HTML or arbitrary JSON is not a
// working MCP server or OpenAI-compatible API.

func mcpStub(t *testing.T, status int, contentType, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestMCPServiceCheckRejectsHTML200(t *testing.T) {
	url := mcpStub(t, http.StatusOK, "text/html", "<html><body>hello</body></html>")
	if err := MCPServiceCheck(context.Background(), url, MCPTransportStreamable); err == nil {
		t.Fatal("an HTML 200 was accepted as MCP health")
	}
}

func TestMCPServiceCheckAcceptsJSONRPCResult(t *testing.T) {
	url := mcpStub(t, http.StatusOK, "application/json",
		`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`)
	if err := MCPServiceCheck(context.Background(), url, MCPTransportStreamable); err != nil {
		t.Fatalf("a valid JSON-RPC result was refused: %v", err)
	}
}

func TestMCPServiceCheckAcceptsJSONRPCError(t *testing.T) {
	// A protocol-valid error still proves the server speaks MCP.
	url := mcpStub(t, http.StatusOK, "application/json",
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`)
	if err := MCPServiceCheck(context.Background(), url, MCPTransportStreamable); err != nil {
		t.Fatalf("a JSON-RPC error answer was refused: %v", err)
	}
}

func TestMCPServiceCheckReportsAuthWallAsReachableNotHealthy(t *testing.T) {
	url := mcpStub(t, http.StatusUnauthorized, "text/plain", "")
	err := MCPServiceCheck(context.Background(), url, MCPTransportStreamable)
	if err == nil {
		t.Fatal("an auth-walled endpoint passed as healthy")
	}
	const want = "requires authentication"
	if len(err.Error()) < len(want) || !contains(err.Error(), want) {
		t.Fatalf("the auth wall error does not distinguish reachability from health: %v", err)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && stringsContains(haystack, needle)
}

func stringsContains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestOpenAIServiceCheckRejectsArbitrary200(t *testing.T) {
	url := mcpStub(t, http.StatusOK, "text/html", "<html>not openai</html>")
	if err := OpenAIServiceCheck(context.Background(), url); err == nil {
		t.Fatal("an arbitrary 200 was accepted as OpenAI-compatible health")
	}
}

func TestOpenAIServiceCheckAcceptsModelsShape(t *testing.T) {
	url := mcpStub(t, http.StatusOK, "application/json",
		`{"data":[{"id":"llama-3"},{"id":"qwen-2"}]}`)
	if err := OpenAIServiceCheck(context.Background(), url); err != nil {
		t.Fatalf("a models-shaped 200 was refused: %v", err)
	}
}

func TestOpenAIServiceCheckJoinsBasePathWithoutDoubleSlash(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)

	// Trailing slash on a base path must not produce //v1/models.
	if err := OpenAIServiceCheck(context.Background(), srv.URL+"/api/"); err != nil {
		t.Fatalf("base-path endpoint refused: %v", err)
	}
	if gotPath != "/api/v1/models" {
		t.Fatalf("models path = %q, want /api/v1/models", gotPath)
	}
}
