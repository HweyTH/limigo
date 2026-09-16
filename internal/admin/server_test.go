package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hweyth/limigo/internal/config"
	adminv1 "github.com/hweyth/limigo/internal/gen/limigo/admin/v1"
	"github.com/hweyth/limigo/internal/rules"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeInspector scripts the engine the Admin API reads from.
type fakeInspector struct {
	rules     []rules.RuleInfo
	decision  rules.Decision
	err       error
	gotRule   string
	gotKey    string
	inspected int
}

func (f *fakeInspector) Rules() []rules.RuleInfo { return f.rules }

func (f *fakeInspector) Inspect(ctx context.Context, ruleName, key string) (rules.Decision, error) {
	f.inspected++
	f.gotRule, f.gotKey = ruleName, key
	return f.decision, f.err
}

func testRules() []rules.RuleInfo {
	return []rules.RuleInfo{
		{Name: "free-tier", HeaderName: "X-Plan", Value: "free", Algorithm: config.FixedWindow, Limit: 100, Window: time.Minute},
		{Name: "burst-tier", HeaderName: "X-Plan", Value: "burst", Algorithm: config.TokenBucket, Limit: 1000, LocalCache: true},
	}
}

func newTestServer(t *testing.T, inspector *fakeInspector, reload Reloader) *Server {
	t.Helper()
	if reload == nil {
		reload = func(context.Context) (int, error) { return len(inspector.rules), nil }
	}
	s, err := NewServer(inspector, reload)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

func TestListRules(t *testing.T) {
	inspector := &fakeInspector{rules: testRules()}
	s := newTestServer(t, inspector, nil)

	resp, err := s.ListRules(context.Background(), &adminv1.ListRulesRequest{})
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(resp.GetRules()) != 2 {
		t.Fatalf("got %d rules, want 2", len(resp.GetRules()))
	}
	first, second := resp.GetRules()[0], resp.GetRules()[1]
	if first.GetName() != "free-tier" || first.GetHeader() != "X-Plan" || first.GetValue() != "free" || first.GetAlgorithm() != "fixed_window" || first.GetLimit() != 100 || first.GetWindow().AsDuration() != time.Minute || first.GetLocalCache() {
		t.Fatalf("first rule = %v", first)
	}
	// A token bucket has no window: the field is unset, not zero.
	if second.GetName() != "burst-tier" || second.GetLimit() != 1000 || second.GetWindow() != nil || !second.GetLocalCache() {
		t.Fatalf("second rule = %v", second)
	}
}

func TestGetQuota(t *testing.T) {
	tests := []struct {
		name       string
		req        *adminv1.GetQuotaRequest
		decision   rules.Decision
		inspectErr error
		wantCode   codes.Code
		check      func(t *testing.T, resp *adminv1.GetQuotaResponse)
	}{
		{
			name:     "reports the store's view",
			req:      &adminv1.GetQuotaRequest{Rule: "free-tier", Key: " user-1 "},
			decision: rules.Decision{Allowed: true, Matched: true, RuleName: "free-tier", Limit: 100, Window: time.Minute, Remaining: 57, Reset: 12 * time.Second},
			check: func(t *testing.T, resp *adminv1.GetQuotaResponse) {
				if resp.GetKey() != "user-1" || resp.GetLimit() != 100 || resp.GetRemaining() != 57 || !resp.GetWouldAllow() {
					t.Fatalf("resp = %v", resp)
				}
				if resp.GetResetAfter().AsDuration() != 12*time.Second {
					t.Fatalf("reset_after = %v, want 12s", resp.GetResetAfter())
				}
				if resp.GetRule().GetName() != "free-tier" || resp.GetRule().GetAlgorithm() != "fixed_window" {
					t.Fatalf("rule = %v", resp.GetRule())
				}
			},
		},
		{
			name:     "no state means no reset",
			req:      &adminv1.GetQuotaRequest{Rule: "burst-tier", Key: "user-1"},
			decision: rules.Decision{Allowed: true, Matched: true, RuleName: "burst-tier", Limit: 1000, Remaining: 1000},
			check: func(t *testing.T, resp *adminv1.GetQuotaResponse) {
				if resp.GetResetAfter() != nil {
					t.Fatalf("reset_after = %v, want unset", resp.GetResetAfter())
				}
				if !resp.GetRule().GetLocalCache() {
					t.Fatal("rule.local_cache = false, want true so the reader knows the view can lag")
				}
			},
		},
		{name: "blank rule", req: &adminv1.GetQuotaRequest{Rule: " ", Key: "k"}, wantCode: codes.InvalidArgument},
		{name: "blank key", req: &adminv1.GetQuotaRequest{Rule: "free-tier", Key: ""}, wantCode: codes.InvalidArgument},
		{name: "unknown rule", req: &adminv1.GetQuotaRequest{Rule: "nope", Key: "k"}, inspectErr: rules.ErrRuleNotFound, wantCode: codes.NotFound},
		{name: "store down", req: &adminv1.GetQuotaRequest{Rule: "free-tier", Key: "k"}, inspectErr: errors.New("redis unreachable"), wantCode: codes.Unavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inspector := &fakeInspector{rules: testRules(), decision: tt.decision, err: tt.inspectErr}
			s := newTestServer(t, inspector, nil)

			resp, err := s.GetQuota(context.Background(), tt.req)
			if tt.wantCode != codes.OK {
				if status.Code(err) != tt.wantCode {
					t.Fatalf("code = %v (%v), want %v", status.Code(err), err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetQuota: %v", err)
			}
			if inspector.gotRule != strings.TrimSpace(tt.req.GetRule()) || inspector.gotKey != strings.TrimSpace(tt.req.GetKey()) {
				t.Fatalf("inspected (%q, %q)", inspector.gotRule, inspector.gotKey)
			}
			tt.check(t, resp)
		})
	}
}

func TestReloadRules(t *testing.T) {
	t.Run("returns the new rule count", func(t *testing.T) {
		calls := 0
		s := newTestServer(t, &fakeInspector{}, func(context.Context) (int, error) {
			calls++
			return 7, nil
		})
		resp, err := s.ReloadRules(context.Background(), &adminv1.ReloadRulesRequest{})
		if err != nil {
			t.Fatalf("ReloadRules: %v", err)
		}
		if resp.GetRuleCount() != 7 || calls != 1 {
			t.Fatalf("rule_count = %d, reload calls = %d", resp.GetRuleCount(), calls)
		}
	})
	t.Run("a bad file is a failed precondition carrying the loader's message", func(t *testing.T) {
		s := newTestServer(t, &fakeInspector{}, func(context.Context) (int, error) {
			return 0, errors.New(`validate config: rule "free-tier": fixed_window.limit must be greater than zero`)
		})
		_, err := s.ReloadRules(context.Background(), &adminv1.ReloadRulesRequest{})
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "free-tier") {
			t.Fatalf("error = %v, want FailedPrecondition naming the rule", err)
		}
	})
}

// TestGateway drives the REST bindings end to end through the in-process
// gateway: the routes admin.proto declares, JSON field names, and the
// gRPC-status-to-HTTP mapping.
func TestGateway(t *testing.T) {
	inspector := &fakeInspector{
		rules:    testRules(),
		decision: rules.Decision{Allowed: false, Matched: true, RuleName: "free-tier", Limit: 100, Window: time.Minute, Remaining: 0, Reset: 30 * time.Second},
	}
	reloads := 0
	s := newTestServer(t, inspector, func(context.Context) (int, error) {
		reloads++
		return 2, nil
	})
	handler, err := NewGatewayHandler(context.Background(), s)
	if err != nil {
		t.Fatalf("NewGatewayHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	get := func(t *testing.T, path string) (int, map[string]any) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return resp.StatusCode, body
	}

	t.Run("GET /v1/admin/rules", func(t *testing.T) {
		code, body := get(t, "/v1/admin/rules")
		if code != http.StatusOK {
			t.Fatalf("status = %d, body = %v", code, body)
		}
		list, _ := body["rules"].([]any)
		if len(list) != 2 {
			t.Fatalf("rules = %v", body["rules"])
		}
		first, _ := list[0].(map[string]any)
		if first["name"] != "free-tier" || first["algorithm"] != "fixed_window" || first["window"] != "60s" {
			t.Fatalf("first rule = %v", first)
		}
	})

	t.Run("GET /v1/admin/rules/{rule}/quota/{key}", func(t *testing.T) {
		code, body := get(t, "/v1/admin/rules/free-tier/quota/user-1")
		if code != http.StatusOK {
			t.Fatalf("status = %d, body = %v", code, body)
		}
		// The gateway emits unpopulated fields, so a zero remaining and a
		// false would_allow are present rather than omitted — a client can
		// tell "denied" from "field missing".
		if body["key"] != "user-1" || body["remaining"] != "0" || body["wouldAllow"] != false || body["resetAfter"] != "30s" {
			t.Fatalf("body = %v", body)
		}
		if body["limit"] != "100" {
			t.Fatalf("limit = %v (int64 is a JSON string)", body["limit"])
		}
	})

	t.Run("unknown rule is 404", func(t *testing.T) {
		inspector.err = rules.ErrRuleNotFound
		defer func() { inspector.err = nil }()
		code, body := get(t, "/v1/admin/rules/nope/quota/user-1")
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, body = %v", code, body)
		}
	})

	t.Run("POST /v1/admin/rules:reload", func(t *testing.T) {
		resp, err := http.Post(srv.URL+"/v1/admin/rules:reload", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.StatusCode != http.StatusOK || body["ruleCount"] != "2" || reloads != 1 {
			t.Fatalf("status = %d, body = %v, reloads = %d", resp.StatusCode, body, reloads)
		}
	})

	t.Run("GET on the reload route is not allowed", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/v1/admin/rules:reload")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 405 or 501", resp.StatusCode)
		}
	})
}

// TestGRPCTransport runs the service over a real gRPC connection (in-memory
// listener) so the generated client, the registration in NewGRPCServer, and
// status propagation are all exercised, not just the handler methods.
func TestGRPCTransport(t *testing.T) {
	inspector := &fakeInspector{
		rules:    testRules(),
		decision: rules.Decision{Allowed: true, Matched: true, RuleName: "free-tier", Limit: 100, Window: time.Minute, Remaining: 99, Reset: 59 * time.Second},
	}
	s := newTestServer(t, inspector, nil)
	grpcServer := NewGRPCServer(s)
	listener := bufconn.Listen(1 << 20)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := adminv1.NewAdminServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	list, err := client.ListRules(ctx, &adminv1.ListRulesRequest{})
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(list.GetRules()) != 2 {
		t.Fatalf("ListRules returned %d rules, want 2", len(list.GetRules()))
	}

	quota, err := client.GetQuota(ctx, &adminv1.GetQuotaRequest{Rule: "free-tier", Key: "user-1"})
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if quota.GetRemaining() != 99 || !quota.GetWouldAllow() || quota.GetResetAfter().AsDuration() != 59*time.Second {
		t.Fatalf("GetQuota = %v", quota)
	}

	inspector.err = rules.ErrRuleNotFound
	if _, err := client.GetQuota(ctx, &adminv1.GetQuotaRequest{Rule: "nope", Key: "user-1"}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetQuota unknown rule: code = %v, want NotFound", status.Code(err))
	}
	inspector.err = nil

	reload, err := client.ReloadRules(ctx, &adminv1.ReloadRulesRequest{})
	if err != nil {
		t.Fatalf("ReloadRules: %v", err)
	}
	if reload.GetRuleCount() != 2 {
		t.Fatalf("ReloadRules rule_count = %d, want 2", reload.GetRuleCount())
	}
}
