package grpcutil

import (
	"context"

	"google.golang.org/grpc/credentials"
)

const headerAuthorize = "authorization"

var _ credentials.PerRPCCredentials = (*unsecureSimpleAuthAccess)(nil)

// unsecureSimpleAuthAccess supplies PerRPCCredentials from a given token.
type unsecureSimpleAuthAccess struct {
	token  string
	scheme string
}

// NewUnsecureSimpleAuthAccess constructs the PerRPCCredentials using a given token.
func NewUnsecureSimpleAuthAccess(scheme, token string) credentials.PerRPCCredentials {
	return &unsecureSimpleAuthAccess{token: token, scheme: scheme}
}

// nolint:revive
func (sa *unsecureSimpleAuthAccess) GetRequestMetadata(_ context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{
		headerAuthorize: "bearer " + sa.token,
	}, nil
}

func (unsecureSimpleAuthAccess) RequireTransportSecurity() bool {
	return false
}
