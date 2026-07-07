package rpc

import (
	"context"
	"crypto/subtle"
	"os"
	"strings"

	"github.com/lesomnus/gantry/cmd/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Auth builds interceptors enforcing serve.auth: a bearer-token whitelist
// (env-expanded, empty tokens dropped) or a verified mTLS client
// certificate. With neither configured every call is allowed. The standard
// health and reflection services stay public — they expose liveness and the
// schema, not the data.
func Auth(cfg config.AuthConfig) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor) {
	tokens := make([][]byte, 0, len(cfg.Tokens))
	for _, t := range cfg.Tokens {
		if t = os.ExpandEnv(t); t != "" {
			tokens = append(tokens, []byte(t))
		}
	}
	mtls := cfg.ClientCA != ""
	enabled := len(tokens) > 0 || mtls

	check := func(ctx context.Context, method string) error {
		if !enabled || isPublicMethod(method) || authorized(ctx, tokens, mtls) {
			return nil
		}
		return status.Error(codes.Unauthenticated, "unauthorized")
	}

	unary := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := check(ctx, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
	stream := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := check(ss.Context(), info.FullMethod); err != nil {
			return err
		}
		return handler(srv, ss)
	}
	return unary, stream
}

func isPublicMethod(method string) bool {
	return strings.HasPrefix(method, "/grpc.health.v1.Health/") ||
		strings.HasPrefix(method, "/grpc.reflection.")
}

func authorized(ctx context.Context, tokens [][]byte, mtls bool) bool {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		for _, v := range md.Get("authorization") {
			raw, ok := strings.CutPrefix(v, "Bearer ")
			if !ok {
				continue
			}
			b := []byte(strings.TrimSpace(raw))
			for _, t := range tokens {
				if subtle.ConstantTimeCompare(b, t) == 1 {
					return true
				}
			}
		}
	}
	if mtls {
		if p, ok := peer.FromContext(ctx); ok && p.AuthInfo != nil {
			if ti, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(ti.State.VerifiedChains) > 0 {
				return true
			}
		}
	}
	return false
}
