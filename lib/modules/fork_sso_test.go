/*
 * Teleport
 * Copyright (C) 2024  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

// Crownpeak fork canary tests.
// These tests guard the OIDC/SAML entitlement bypass that allows SSO
// authentication in OSS builds. If any of these fail, the fork patch
// has been lost or broken during a rebase.
package modules_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/entitlements"
	"github.com/gravitational/teleport/lib/modules"
)

// TestForkOIDCSAMLEntitlementAlwaysEnabled verifies the Crownpeak fork patch:
// OIDC and SAML must be enabled on any Features value, including a zero value,
// so that OSS builds can create OIDC/SAML connectors without Enterprise licensing.
func TestForkOIDCSAMLEntitlementAlwaysEnabled(t *testing.T) {
	t.Parallel()

	f := modules.Features{} // zero value — no entitlements set at all

	// OIDC must be force-enabled.
	oidc := f.GetEntitlement(entitlements.OIDC)
	require.True(t, oidc.Enabled, "OIDC entitlement must be force-enabled in OSS build (fork patch missing?)")
	require.Equal(t, int32(0), oidc.Limit)

	// SAML must be force-enabled.
	saml := f.GetEntitlement(entitlements.SAML)
	require.True(t, saml.Enabled, "SAML entitlement must be force-enabled in OSS build (fork patch missing?)")
	require.Equal(t, int32(0), saml.Limit)
}

// TestForkEntitlementScopeGuard verifies the patch did not accidentally
// enable all entitlements — only OIDC and SAML should be force-enabled.
func TestForkEntitlementScopeGuard(t *testing.T) {
	t.Parallel()

	f := modules.Features{} // zero value

	notEnabled := []entitlements.EntitlementKind{
		entitlements.HSM,
		entitlements.Policy,
		entitlements.AccessLists,
		entitlements.DeviceTrust,
		entitlements.K8s,
	}
	for _, e := range notEnabled {
		result := f.GetEntitlement(e)
		require.False(t, result.Enabled, "entitlement %v should NOT be force-enabled (fork patch too broad?)", e)
	}
}

// TestForkGetProtoEntitlementOIDCSAML verifies GetProtoEntitlement also
// force-enables OIDC and SAML, since the proxy uses this path.
func TestForkGetProtoEntitlementOIDCSAML(t *testing.T) {
	t.Parallel()

	pf := &proto.Features{} // zero proto — no entitlements

	oidc := modules.GetProtoEntitlement(pf, entitlements.OIDC)
	require.NotNil(t, oidc)
	require.True(t, oidc.Enabled, "GetProtoEntitlement OIDC must be force-enabled (fork patch missing?)")

	saml := modules.GetProtoEntitlement(pf, entitlements.SAML)
	require.NotNil(t, saml)
	require.True(t, saml.Enabled, "GetProtoEntitlement SAML must be force-enabled (fork patch missing?)")
}

// TestForkOIDCAuthRequestNonceField guards the proto nonce field added by the fork.
// The field must exist with protobuf tag "bytes,27" and json tag "nonce".
// This broke once during the 18.5.1->18.7.6 rebase when upstream took field
// numbers 25 (login_hint) and 26 (scope), requiring nonce to move to 27.
func TestForkOIDCAuthRequestNonceField(t *testing.T) {
	t.Parallel()

	// Field must be settable and readable.
	req := &types.OIDCAuthRequest{Nonce: "test-nonce-value"}
	require.Equal(t, "test-nonce-value", req.Nonce, "OIDCAuthRequest.Nonce field missing (fork patch dropped?)")

	// Verify the proto field number (27) and json tag via struct reflection.
	// The generated protobuf tag encodes the field number: "bytes,27,opt,..."
	rt := reflect.TypeOf(types.OIDCAuthRequest{})
	f, ok := rt.FieldByName("Nonce")
	require.True(t, ok, "OIDCAuthRequest.Nonce struct field not found")
	protoTag := f.Tag.Get("protobuf")
	require.Contains(t, protoTag, ",27,", "OIDCAuthRequest.Nonce must be proto field 27 (upstream took 25/26 in v18.7.x), got: %s", protoTag)
	require.Equal(t, "nonce", f.Tag.Get("json"), "OIDCAuthRequest.Nonce json tag must be \"nonce\"")
}
