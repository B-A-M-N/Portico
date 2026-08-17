package tunnel

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

func errorResp(status int, code int64, message string) func() (int, string) {
	return func() (int, string) {
		return status, `{"success":false,"errors":[{"code":` + itoa(code) + `,"message":"` + message + `"}],"result":null}`
	}
}

func TestTunnelGetClassifies401AsUnauthorized(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/accounts": errorResp(http.StatusUnauthorized, 7000, "Invalid API Token"),
	})
	_, err := NewAPIManager(api).Get(context.Background(), "acct-1", "tun-1")
	if err == nil {
		t.Fatal("expected an error for 401")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("401 should be unauthorized: %v", err)
	}
}

func TestTunnelGetClassifies403AsUnauthorized(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/accounts": errorResp(http.StatusForbidden, 10000, "Authentication error"),
	})
	_, err := NewAPIManager(api).Get(context.Background(), "acct-1", "tun-1")
	if err == nil {
		t.Fatal("expected an error for 403")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("403 should be unauthorized: %v", err)
	}
}

func TestTunnelGetClassifies429AsRateLimited(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/accounts": errorResp(http.StatusTooManyRequests, 7003, "Rate limited"),
	})
	_, err := NewAPIManager(api).Get(context.Background(), "acct-1", "tun-1")
	if err == nil {
		t.Fatal("expected an error for 429")
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("429 should be rate limited: %v", err)
	}
}

func TestTunnelGetClassifies5xxAsTransient(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(itoa(int64(status)), func(t *testing.T) {
			api, _ := newFakeAPI(t, map[string]func() (int, string){
				"GET /client/v4/accounts": errorResp(status, 5000, "Internal error"),
			})
			_, err := NewAPIManager(api).Get(context.Background(), "acct-1", "tun-1")
			if err == nil {
				t.Fatal("expected an error for 5xx")
			}
			if !strings.Contains(err.Error(), "transient") {
				t.Fatalf("5xx should be transient: %v", err)
			}
		})
	}
}

func TestTunnelGetHandlesMalformedJSON(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/accounts": func() (int, string) {
			return http.StatusOK, `{"success":true,"result":}`
		},
	})
	_, err := NewAPIManager(api).Get(context.Background(), "acct-1", "tun-1")
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestTunnelGetHandlesContextCancellation(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/accounts": func() (int, string) {
			return http.StatusOK, `{"success":true,"result":{"id":"tun-1","name":"test","status":"active"}}`
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewAPIManager(api).Get(ctx, "acct-1", "tun-1")
	if err == nil {
		t.Fatal("expected an error for cancelled context")
	}
}

func TestTunnelDeleteClassifies404AsMissing(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"DELETE /client/v4/accounts": errorResp(http.StatusNotFound, 1049, "Tunnel not found"),
	})
	err := NewAPIManager(api).Delete(context.Background(), "acct-1", "tun-gone")
	if err != nil {
		t.Fatalf("deleting a missing tunnel should not error: %v", err)
	}
}

func TestTunnelDeleteClassifies403AsUnauthorized(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"DELETE /client/v4/accounts": errorResp(http.StatusForbidden, 10000, "Authentication error"),
	})
	err := NewAPIManager(api).Delete(context.Background(), "acct-1", "tun-1")
	if err == nil {
		t.Fatal("expected an error for 403 during delete")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("403 during delete should be unauthorized: %v", err)
	}
}

func TestTunnelDeleteClassifies429AsTransient(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"DELETE /client/v4/accounts": errorResp(http.StatusTooManyRequests, 7003, "Rate limited"),
	})
	err := NewAPIManager(api).Delete(context.Background(), "acct-1", "tun-1")
	if err == nil {
		t.Fatal("expected an error for 429 during delete")
	}
	if !strings.Contains(err.Error(), "transient") {
		t.Fatalf("429 during delete should be transient: %v", err)
	}
}

func TestTunnelCreateClassifies401AsUnauthorized(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"POST /client/v4/accounts": errorResp(http.StatusUnauthorized, 7000, "Invalid API Token"),
	})
	_, err := NewAPIManager(api).Create(context.Background(), "acct-1", "portico-test")
	if err == nil {
		t.Fatal("expected an error for 401 during creation")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("401 during creation should be unauthorized: %v", err)
	}
}

func TestTunnelCreateClassifies403AsUnauthorized(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"POST /client/v4/accounts": errorResp(http.StatusForbidden, 10000, "Authentication error"),
	})
	_, err := NewAPIManager(api).Create(context.Background(), "acct-1", "portico-test")
	if err == nil {
		t.Fatal("expected an error for 403 during creation")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("403 during creation should be unauthorized: %v", err)
	}
}

func TestTunnelCreateClassifies429AsTransient(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"POST /client/v4/accounts": errorResp(http.StatusTooManyRequests, 7003, "Rate limited"),
	})
	_, err := NewAPIManager(api).Create(context.Background(), "acct-1", "portico-test")
	if err == nil {
		t.Fatal("expected an error for 429 during creation")
	}
	if !strings.Contains(err.Error(), "transient") {
		t.Fatalf("429 during creation should be transient: %v", err)
	}
}

func TestTunnelCreateClassifies5xxAsTransient(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"POST /client/v4/accounts": errorResp(http.StatusBadGateway, 5000, "Bad Gateway"),
	})
	_, err := NewAPIManager(api).Create(context.Background(), "acct-1", "portico-test")
	if err == nil {
		t.Fatal("expected an error for 502 during creation")
	}
	if !strings.Contains(err.Error(), "transient") {
		t.Fatalf("502 during creation should be transient: %v", err)
	}
}

func TestTunnelCreateHandlesMalformedJSON(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"POST /client/v4/accounts": func() (int, string) {
			return http.StatusOK, `{"success":true,"result":}`
		},
	})
	_, err := NewAPIManager(api).Create(context.Background(), "acct-1", "portico-test")
	if err == nil {
		t.Fatal("expected an error for malformed JSON during creation")
	}
}

func TestTunnelCreateHandlesContextCancellation(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"POST /client/v4/accounts": func() (int, string) {
			return http.StatusOK, `{"success":true,"result":{"id":"tun-1","name":"test","status":"inactive"}}`
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewAPIManager(api).Create(ctx, "acct-1", "portico-test")
	if err == nil {
		t.Fatal("expected an error for cancelled context during creation")
	}
}
