// Package admin implements the Limigo Admin API (proto/limigo/admin/v1): a
// gRPC service with a REST gateway that lists the rule set a node enforces,
// inspects a key's quota without consuming it, and triggers a rules reload.
// It is a control plane: it never shares a listener with /v1/check, and it
// reads through the same engine the data plane uses rather than keeping any
// state of its own.
package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	adminv1 "github.com/hweyth/limigo/internal/gen/limigo/admin/v1"
	"github.com/hweyth/limigo/internal/rules"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Inspector is the read side of the engine the Admin API answers from.
// rules.EngineHolder satisfies it, so every answer describes the rule set
// currently serving /v1/check on this node.
type Inspector interface {
	Rules() []rules.RuleInfo
	Inspect(ctx context.Context, ruleName, key string) (rules.Decision, error)
}

// Reloader re-reads the node's config file and swaps the compiled rule set,
// returning the number of rules now active. main wires the same function the
// file watcher calls, so a reload through the API and a reload from a file
// change are one code path with one ordering (flush local caches, then swap).
type Reloader func(ctx context.Context) (ruleCount int, err error)

// Server implements adminv1.AdminServiceServer.
type Server struct {
	adminv1.UnimplementedAdminServiceServer
	inspector Inspector
	reload    Reloader
}

// NewServer returns a Server answering from inspector and reloading through
// reload. Both must be non-nil.
func NewServer(inspector Inspector, reload Reloader) (*Server, error) {
	if inspector == nil {
		return nil, fmt.Errorf("inspector must not be nil")
	}
	if reload == nil {
		return nil, fmt.Errorf("reloader must not be nil")
	}
	return &Server{inspector: inspector, reload: reload}, nil
}

// ListRules returns the rule set in match order.
func (s *Server) ListRules(ctx context.Context, req *adminv1.ListRulesRequest) (*adminv1.ListRulesResponse, error) {
	infos := s.inspector.Rules()
	resp := &adminv1.ListRulesResponse{Rules: make([]*adminv1.Rule, 0, len(infos))}
	for _, info := range infos {
		resp.Rules = append(resp.Rules, ruleMessage(info))
	}
	return resp, nil
}

// GetQuota reads one key's quota under one rule without consuming it.
// An unknown rule is NOT_FOUND; a blank rule or key is INVALID_ARGUMENT; a
// store that cannot be reached is UNAVAILABLE, mirroring the data plane's
// fail-closed 503 for the same condition.
func (s *Server) GetQuota(ctx context.Context, req *adminv1.GetQuotaRequest) (*adminv1.GetQuotaResponse, error) {
	rule := strings.TrimSpace(req.GetRule())
	key := strings.TrimSpace(req.GetKey())
	if rule == "" {
		return nil, status.Error(codes.InvalidArgument, "rule must not be empty")
	}
	if key == "" {
		return nil, status.Error(codes.InvalidArgument, "key must not be empty")
	}

	decision, err := s.inspector.Inspect(ctx, rule, key)
	if err != nil {
		if errors.Is(err, rules.ErrRuleNotFound) {
			return nil, status.Errorf(codes.NotFound, "rule %q is not configured", rule)
		}
		return nil, status.Errorf(codes.Unavailable, "inspect %q for key %q: %v", rule, key, err)
	}

	var info rules.RuleInfo
	for _, candidate := range s.inspector.Rules() {
		if candidate.Name == rule {
			info = candidate
			break
		}
	}
	resp := &adminv1.GetQuotaResponse{
		Rule:       ruleMessage(info),
		Key:        key,
		Limit:      decision.Limit,
		Remaining:  decision.Remaining,
		WouldAllow: decision.Allowed,
	}
	if decision.Reset > 0 {
		resp.ResetAfter = durationpb.New(decision.Reset)
	}
	return resp, nil
}

// ReloadRules re-reads the config file and swaps the rule set. A file that
// fails to load, validate or compile leaves the running rules untouched and
// is reported as FAILED_PRECONDITION with the loader's message, which names
// the offending rule.
func (s *Server) ReloadRules(ctx context.Context, req *adminv1.ReloadRulesRequest) (*adminv1.ReloadRulesResponse, error) {
	count, err := s.reload(ctx)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "reload rules: %v", err)
	}
	return &adminv1.ReloadRulesResponse{RuleCount: int64(count)}, nil
}

// ruleMessage converts a compiled rule's description to its wire form.
func ruleMessage(info rules.RuleInfo) *adminv1.Rule {
	msg := &adminv1.Rule{
		Name:       info.Name,
		Header:     info.HeaderName,
		Value:      info.Value,
		Algorithm:  string(info.Algorithm),
		Limit:      info.Limit,
		LocalCache: info.LocalCache,
	}
	if info.Window > 0 {
		msg.Window = durationpb.New(info.Window)
	}
	return msg
}

// NewGRPCServer returns a gRPC server with s registered, ready to Serve on a
// listener of the caller's choosing.
func NewGRPCServer(s *Server) *grpc.Server {
	srv := grpc.NewServer()
	adminv1.RegisterAdminServiceServer(srv, s)
	return srv
}

// NewGatewayHandler returns the REST gateway for s as an http.Handler. The
// gateway calls s in-process — no loopback gRPC connection — so the REST
// listener is exactly as available as the gRPC one and needs no dial
// configuration. Routes are the google.api.http bindings in admin.proto:
// GET /v1/admin/rules, GET /v1/admin/rules/{rule}/quota/{key}, and
// POST /v1/admin/rules:reload.
func NewGatewayHandler(ctx context.Context, s *Server) (http.Handler, error) {
	mux := runtime.NewServeMux()
	if err := adminv1.RegisterAdminServiceHandlerServer(ctx, mux, s); err != nil {
		return nil, fmt.Errorf("register admin gateway: %w", err)
	}
	return mux, nil
}
