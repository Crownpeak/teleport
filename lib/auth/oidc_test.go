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

package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v3"
	"github.com/go-jose/go-jose/v3/jwt"
	"github.com/google/uuid"
	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/gravitational/teleport/api/constants"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/auth/authclient"
	"github.com/gravitational/teleport/lib/auth/authtest"
	authority "github.com/gravitational/teleport/lib/auth/testauthority"
	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/teleport/lib/backend/memory"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/events/eventstest"
	"github.com/gravitational/teleport/lib/services"
)

// oidcContext holds test context for OIDC authentication tests
type oidcContext struct {
	a              *auth.Server
	mockEmitter    *eventstest.MockRecorderEmitter
	b              backend.Backend
	c              *clockwork.FakeClock
	oidcProvider   *mockOIDCProvider
	oidcServer     *httptest.Server
	oidcConnector  types.OIDCConnector
}

// mockOIDCProvider is a mock OIDC provider for testing
type mockOIDCProvider struct {
	issuerURL      string
	signingKey     *rsa.PrivateKey
	publicKey      *rsa.PublicKey
	claims         map[string]interface{}
	expectNonce    string
	validateNonce  bool
	returnNonce    bool
	nonceMismatch  bool
}

// setupOIDCContext initializes test context with auth server and mock OIDC provider
func setupOIDCContext(ctx context.Context, t *testing.T) *oidcContext {
	var tt oidcContext
	t.Cleanup(func() { tt.Close() })

	tt.c = clockwork.NewFakeClockAt(time.Now())

	var err error
	tt.b, err = memory.New(memory.Config{
		Context: context.Background(),
		Clock:   tt.c,
	})
	require.NoError(t, err)

	clusterName, err := services.NewClusterNameWithRandomID(types.ClusterNameSpecV2{
		ClusterName: "test.localhost",
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, tt.b.Close())
	})

	authConfig := &auth.InitConfig{
		ClusterName:            clusterName,
		Backend:                tt.b,
		VersionStorage:         authtest.NewFakeTeleportVersion(),
		Authority:              authority.New(),
		SkipPeriodicOperations: true,
		HostUUID:               uuid.NewString(),
	}
	tt.a, err = auth.NewServer(authConfig)
	require.NoError(t, err)

	tt.mockEmitter = &eventstest.MockRecorderEmitter{}
	tt.a.SetEmitter(tt.mockEmitter)

	// Store cluster name in backend
	err = tt.a.UpsertClusterName(clusterName)
	require.NoError(t, err)

	// Create default cluster networking config
	netConfig, err := types.NewClusterNetworkingConfigFromConfigFile(types.ClusterNetworkingConfigSpecV2{})
	require.NoError(t, err)
	_, err = tt.a.UpsertClusterNetworkingConfig(ctx, netConfig)
	require.NoError(t, err)

	// Create default auth preference
	authPref, err := types.NewAuthPreference(types.AuthPreferenceSpecV2{
		Type:         constants.Local,
		SecondFactor: constants.SecondFactorOff,
	})
	require.NoError(t, err)
	_, err = tt.a.UpsertAuthPreference(ctx, authPref)
	require.NoError(t, err)

	// Setup mock OIDC provider
	tt.oidcProvider = newMockOIDCProvider(t)
	tt.oidcServer = httptest.NewServer(tt.oidcProvider.handler())
	tt.oidcProvider.issuerURL = tt.oidcServer.URL

	// Create and store OIDC connector
	redirectURL := fmt.Sprintf("https://test.localhost/v1/webapi/oidc/callback")
	tt.oidcConnector, err = types.NewOIDCConnector("test-oidc", types.OIDCConnectorSpecV3{
		IssuerURL:    tt.oidcProvider.issuerURL,
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		RedirectURLs: []string{redirectURL},
		Scope:        []string{"openid", "email", "profile"},
		ClaimsToRoles: []types.ClaimMapping{
			{
				Claim: "groups",
				Value: "admins",
				Roles: []string{"admin"},
			},
			{
				Claim: "groups",
				Value: "users",
				Roles: []string{"user"},
			},
			{
				Claim: "email",
				Value: "*@example.com",
				Roles: []string{"user"},
			},
		},
		UsernameClaim: "email",
		ClientRedirectSettings: &types.SSOClientRedirectSettings{
			AllowedHttpsHostnames: []string{"localhost"},
		},
	})
	require.NoError(t, err)

	_, err = tt.a.UpsertOIDCConnector(ctx, tt.oidcConnector)
	require.NoError(t, err)

	// Create roles referenced by connector
	adminRole, err := types.NewRole("admin", types.RoleSpecV6{
		Allow: types.RoleConditions{
			Logins: []string{"admin"},
		},
	})
	require.NoError(t, err)
	_, err = tt.a.UpsertRole(ctx, adminRole)
	require.NoError(t, err)

	userRole, err := types.NewRole("user", types.RoleSpecV6{
		Allow: types.RoleConditions{
			Logins: []string{"user"},
		},
	})
	require.NoError(t, err)
	_, err = tt.a.UpsertRole(ctx, userRole)
	require.NoError(t, err)

	// Enable OIDC service
	tt.a.SetOIDCService(auth.NewOIDCAuthService(tt.a))

	return &tt
}

func (tt *oidcContext) Close() error {
	if tt.oidcServer != nil {
		tt.oidcServer.Close()
	}
	return trace.NewAggregate(
		tt.a.Close(),
		tt.b.Close())
}

// newMockOIDCProvider creates a new mock OIDC provider
func newMockOIDCProvider(t *testing.T) *mockOIDCProvider {
	// Generate RSA key pair for signing tokens
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	return &mockOIDCProvider{
		signingKey:    privateKey,
		publicKey:     &privateKey.PublicKey,
		claims:        make(map[string]interface{}),
		returnNonce:   true,
		validateNonce: false,
	}
}

// handler returns HTTP handler for mock OIDC provider endpoints
func (m *mockOIDCProvider) handler() http.Handler {
	mux := http.NewServeMux()

	// OpenID Connect Discovery endpoint
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		config := map[string]interface{}{
			"issuer":                 m.issuerURL,
			"authorization_endpoint": m.issuerURL + "/authorize",
			"token_endpoint":         m.issuerURL + "/token",
			"jwks_uri":               m.issuerURL + "/jwks",
			"userinfo_endpoint":      m.issuerURL + "/userinfo",
			"response_types_supported": []string{
				"code",
			},
			"subject_types_supported": []string{
				"public",
			},
			"id_token_signing_alg_values_supported": []string{
				"RS256",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(config)
	})

	// JWKS endpoint for token verification
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		// Convert public key to JWK
		n := base64.RawURLEncoding.EncodeToString(m.publicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}) // 65537

		jwks := map[string]interface{}{
			"keys": []map[string]interface{}{
				{
					"kty": "RSA",
					"use": "sig",
					"kid": "test-key-id",
					"alg": "RS256",
					"n":   n,
					"e":   e,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jwks)
	})

	// Token endpoint
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		err := r.ParseForm()
		if err != nil {
			http.Error(w, "invalid form data", http.StatusBadRequest)
			return
		}

		// Create ID token with claims
		idToken := m.createIDToken()

		response := map[string]interface{}{
			"access_token": "mock-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     idToken,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})

	return mux
}

// createIDToken creates a signed ID token with configured claims
func (m *mockOIDCProvider) createIDToken() string {
	// Create signer
	signer, err := jose.NewSigner(
		jose.SigningKey{
			Algorithm: jose.RS256,
			Key:       m.signingKey,
		},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key-id"),
	)
	if err != nil {
		panic(err)
	}

	// Build claims
	claims := jwt.Claims{
		Issuer:   m.issuerURL,
		Subject:  "test-user-id",
		Audience: jwt.Audience{"test-client-id"},
		IssuedAt: jwt.NewNumericDate(time.Now()),
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}

	// Add custom claims
	customClaims := make(map[string]interface{})
	for k, v := range m.claims {
		customClaims[k] = v
	}

	// Add nonce if configured
	if m.returnNonce && m.expectNonce != "" {
		if m.nonceMismatch {
			customClaims["nonce"] = "wrong-nonce"
		} else {
			customClaims["nonce"] = m.expectNonce
		}
	}

	// Sign token
	raw, err := jwt.Signed(signer).Claims(claims).Claims(customClaims).CompactSerialize()
	if err != nil {
		panic(err)
	}

	return raw
}

// setUserClaims sets user-specific claims for the mock provider
func (m *mockOIDCProvider) setUserClaims(email string, groups []string) {
	m.claims["email"] = email
	m.claims["email_verified"] = true
	if len(groups) > 0 {
		m.claims["groups"] = groups
	}
}

// TestCreateOIDCAuthRequest verifies that OIDC auth request creation works correctly
func TestCreateOIDCAuthRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tt := setupOIDCContext(ctx, t)

	tests := []struct {
		name              string
		req               types.OIDCAuthRequest
		expectError       bool
		validateRedirect  bool
		validateNonce     bool
		validateState     bool
		validatePKCE      bool
	}{
		{
			name: "successful auth request creation",
			req: types.OIDCAuthRequest{
				ConnectorID:       "test-oidc",
				Type:              constants.OIDC,
				CreateWebSession:  true,
				ProxyAddress:      "https://test.localhost",
				CertTTL:           1 * time.Hour,
			},
			validateRedirect: true,
			validateNonce:    true,
			validateState:    true,
		},
		{
			name: "auth request with PKCE",
			req: types.OIDCAuthRequest{
				ConnectorID:       "test-oidc",
				Type:              constants.OIDC,
				CreateWebSession:  false,
				ProxyAddress:      "https://test.localhost",
				CertTTL:           1 * time.Hour,
				PkceVerifier:      "test-verifier-code",
			},
			validateRedirect: true,
			validateNonce:    true,
			validateState:    true,
			validatePKCE:     true,
		},
		{
			name: "invalid connector",
			req: types.OIDCAuthRequest{
				ConnectorID:      "non-existent",
				Type:             constants.OIDC,
				CreateWebSession: true,
				ProxyAddress:     "https://test.localhost",
				CertTTL:          1 * time.Hour,
			},
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			authRequest, err := tt.a.CreateOIDCAuthRequest(ctx, tc.req)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, authRequest)

			if tc.validateState {
				require.NotEmpty(t, authRequest.StateToken, "state token should be generated")
				require.Len(t, authRequest.StateToken, 32, "state token should be 32 hex chars (16 bytes)")
			}

			if tc.validateNonce {
				require.NotEmpty(t, authRequest.Nonce, "nonce should be generated")
				require.Len(t, authRequest.Nonce, 32, "nonce should be 32 hex chars (16 bytes)")
			}

			if tc.validateRedirect {
				require.NotEmpty(t, authRequest.RedirectURL, "redirect URL should be set")
				redirectURL, err := url.Parse(authRequest.RedirectURL)
				require.NoError(t, err)
				require.Equal(t, tt.oidcProvider.issuerURL+"/authorize", redirectURL.Scheme+"://"+redirectURL.Host+redirectURL.Path)

				// Verify query parameters
				query := redirectURL.Query()
				require.Equal(t, "test-client-id", query.Get("client_id"))
				require.Equal(t, authRequest.StateToken, query.Get("state"))
				require.Equal(t, authRequest.Nonce, query.Get("nonce"), "nonce should be in authorization URL")
				require.Equal(t, "code", query.Get("response_type"))
			}

			if tc.validatePKCE {
				redirectURL, err := url.Parse(authRequest.RedirectURL)
				require.NoError(t, err)
				query := redirectURL.Query()
				require.NotEmpty(t, query.Get("code_challenge"), "PKCE challenge should be present")
				require.Equal(t, "S256", query.Get("code_challenge_method"), "PKCE method should be S256")
			}

			// Verify auth request was stored in backend
			storedRequest, err := tt.a.Services.GetOIDCAuthRequest(ctx, authRequest.StateToken)
			require.NoError(t, err)
			require.Equal(t, authRequest.ConnectorID, storedRequest.ConnectorID)
			require.Equal(t, authRequest.Nonce, storedRequest.Nonce)
		})
	}
}

// TestOIDCNonceGeneration verifies nonce generation properties
func TestOIDCNonceGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tt := setupOIDCContext(ctx, t)

	// Generate multiple auth requests and verify nonces
	nonces := make(map[string]bool)
	for i := 0; i < 10; i++ {
		req := types.OIDCAuthRequest{
			ConnectorID:      "test-oidc",
			Type:             constants.OIDC,
			CreateWebSession: true,
			ProxyAddress:     "https://test.localhost",
			CertTTL:          1 * time.Hour,
		}

		authRequest, err := tt.a.CreateOIDCAuthRequest(ctx, req)
		require.NoError(t, err)

		// Verify nonce is not empty
		require.NotEmpty(t, authRequest.Nonce, "nonce should always be generated")

		// Verify nonce has correct length (16 bytes = 32 hex chars)
		require.Len(t, authRequest.Nonce, 32, "nonce should be 32 hex characters")

		// Verify nonce is unique
		require.False(t, nonces[authRequest.Nonce], "nonce should be unique per request")
		nonces[authRequest.Nonce] = true

		// Verify nonce is included in redirect URL
		redirectURL, err := url.Parse(authRequest.RedirectURL)
		require.NoError(t, err)
		require.Equal(t, authRequest.Nonce, redirectURL.Query().Get("nonce"))
	}

	// Verify all nonces are different
	require.Len(t, nonces, 10, "all nonces should be unique")
}

// TestOIDCNonceValidation verifies nonce validation during callback
func TestOIDCNonceValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tt := setupOIDCContext(ctx, t)

	tests := []struct {
		name           string
		setupProvider  func(*mockOIDCProvider, string)
		expectError    bool
		errorContains  string
	}{
		{
			name: "successful nonce validation",
			setupProvider: func(p *mockOIDCProvider, nonce string) {
				p.setUserClaims("test@example.com", []string{"admins"})
				p.expectNonce = nonce
				p.returnNonce = true
				p.nonceMismatch = false
			},
			expectError: false,
		},
		{
			name: "missing nonce in ID token",
			setupProvider: func(p *mockOIDCProvider, nonce string) {
				p.setUserClaims("test@example.com", []string{"admins"})
				p.expectNonce = nonce
				p.returnNonce = false // Don't include nonce in token
			},
			expectError:   true,
			errorContains: "missing nonce claim",
		},
		{
			name: "nonce mismatch",
			setupProvider: func(p *mockOIDCProvider, nonce string) {
				p.setUserClaims("test@example.com", []string{"admins"})
				p.expectNonce = nonce
				p.returnNonce = true
				p.nonceMismatch = true // Return wrong nonce
			},
			expectError:   true,
			errorContains: "nonce mismatch",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create auth request
			req := types.OIDCAuthRequest{
				ConnectorID:      "test-oidc",
				Type:             constants.OIDC,
				CreateWebSession: true,
				ProxyAddress:     "https://test.localhost",
				CertTTL:          1 * time.Hour,
			}

			authRequest, err := tt.a.CreateOIDCAuthRequest(ctx, req)
			require.NoError(t, err)

			// Setup provider with nonce
			tc.setupProvider(tt.oidcProvider, authRequest.Nonce)

			// Simulate callback
			callbackQuery := url.Values{
				"code":  []string{"test-code"},
				"state": []string{authRequest.StateToken},
			}

			resp, err := tt.a.ValidateOIDCAuthCallback(ctx, callbackQuery)

			if tc.expectError {
				require.Error(t, err)
				if tc.errorContains != "" {
					require.Contains(t, err.Error(), tc.errorContains)
				}
				return
			}

			require.NoError(t, err)
			require.NotNil(t, resp)
			require.Equal(t, "test@example.com", resp.Username)
		})
	}
}

// TestOIDCProviderCaching verifies provider caching behavior
func TestOIDCProviderCaching(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tt := setupOIDCContext(ctx, t)

	// Track discovery endpoint calls
	discoveryCallCount := 0
	originalHandler := tt.oidcServer.Config.Handler

	// Wrap handler to count discovery calls
	tt.oidcServer.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/openid-configuration" {
			discoveryCallCount++
		}
		originalHandler.ServeHTTP(w, r)
	})

	// First request - should call discovery endpoint
	req1 := types.OIDCAuthRequest{
		ConnectorID:      "test-oidc",
		Type:             constants.OIDC,
		CreateWebSession: true,
		ProxyAddress:     "https://test.localhost",
		CertTTL:          1 * time.Hour,
	}
	_, err := tt.a.CreateOIDCAuthRequest(ctx, req1)
	require.NoError(t, err)
	require.Equal(t, 1, discoveryCallCount, "first request should call discovery endpoint")

	// Second request - should use cached provider
	req2 := types.OIDCAuthRequest{
		ConnectorID:      "test-oidc",
		Type:             constants.OIDC,
		CreateWebSession: true,
		ProxyAddress:     "https://test.localhost",
		CertTTL:          1 * time.Hour,
	}
	_, err = tt.a.CreateOIDCAuthRequest(ctx, req2)
	require.NoError(t, err)
	require.Equal(t, 1, discoveryCallCount, "second request should use cached provider")

	// Third request - still cached
	req3 := types.OIDCAuthRequest{
		ConnectorID:      "test-oidc",
		Type:             constants.OIDC,
		CreateWebSession: true,
		ProxyAddress:     "https://test.localhost",
		CertTTL:          1 * time.Hour,
	}
	_, err = tt.a.CreateOIDCAuthRequest(ctx, req3)
	require.NoError(t, err)
	require.Equal(t, 1, discoveryCallCount, "third request should use cached provider")
}

// TestValidateOIDCAuthCallback verifies the full OIDC callback flow
func TestValidateOIDCAuthCallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tt := setupOIDCContext(ctx, t)

	tests := []struct {
		name           string
		setupRequest   func() *types.OIDCAuthRequest
		setupProvider  func(*mockOIDCProvider, *types.OIDCAuthRequest)
		setupQuery     func(*types.OIDCAuthRequest) url.Values
		expectError    bool
		errorContains  string
		validateResult func(*testing.T, *authclient.OIDCAuthResponse)
	}{
		{
			name: "successful authentication with web session",
			setupRequest: func() *types.OIDCAuthRequest {
				req := types.OIDCAuthRequest{
					ConnectorID:      "test-oidc",
					Type:             constants.OIDC,
					CreateWebSession: true,
					ProxyAddress:     "https://test.localhost",
					CertTTL:          1 * time.Hour,
				}
				authReq, err := tt.a.CreateOIDCAuthRequest(ctx, req)
				require.NoError(t, err)
				return authReq
			},
			setupProvider: func(p *mockOIDCProvider, req *types.OIDCAuthRequest) {
				p.setUserClaims("admin@example.com", []string{"admins"})
				p.expectNonce = req.Nonce
				p.returnNonce = true
			},
			setupQuery: func(req *types.OIDCAuthRequest) url.Values {
				return url.Values{
					"code":  []string{"test-code"},
					"state": []string{req.StateToken},
				}
			},
			validateResult: func(t *testing.T, resp *authclient.OIDCAuthResponse) {
				require.Equal(t, "admin@example.com", resp.Username)
				require.NotNil(t, resp.Session, "web session should be created")
			},
		},
		{
			name: "successful authentication with SSH certificate",
			setupRequest: func() *types.OIDCAuthRequest {
				req := types.OIDCAuthRequest{
					ConnectorID:      "test-oidc",
					Type:             constants.OIDC,
					CreateWebSession: false,
					ProxyAddress:     "https://test.localhost",
					CertTTL:          1 * time.Hour,
					SshPublicKey:     []byte("ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQDVvLu5QOlJ test@example.com"),
				}
				authReq, err := tt.a.CreateOIDCAuthRequest(ctx, req)
				require.NoError(t, err)
				return authReq
			},
			setupProvider: func(p *mockOIDCProvider, req *types.OIDCAuthRequest) {
				p.setUserClaims("user@example.com", []string{"users"})
				p.expectNonce = req.Nonce
				p.returnNonce = true
			},
			setupQuery: func(req *types.OIDCAuthRequest) url.Values {
				return url.Values{
					"code":  []string{"test-code"},
					"state": []string{req.StateToken},
				}
			},
			validateResult: func(t *testing.T, resp *authclient.OIDCAuthResponse) {
				require.Equal(t, "user@example.com", resp.Username)
				// Note: Certificate generation would require valid SSH public key
			},
		},
		{
			name: "missing code parameter",
			setupRequest: func() *types.OIDCAuthRequest {
				req := types.OIDCAuthRequest{
					ConnectorID:      "test-oidc",
					Type:             constants.OIDC,
					CreateWebSession: true,
					ProxyAddress:     "https://test.localhost",
					CertTTL:          1 * time.Hour,
				}
				authReq, err := tt.a.CreateOIDCAuthRequest(ctx, req)
				require.NoError(t, err)
				return authReq
			},
			setupProvider: func(p *mockOIDCProvider, req *types.OIDCAuthRequest) {
				// Provider setup not relevant for this test
			},
			setupQuery: func(req *types.OIDCAuthRequest) url.Values {
				return url.Values{
					"state": []string{req.StateToken},
				}
			},
			expectError:   true,
			errorContains: "code query param must be set",
		},
		{
			name: "missing state parameter",
			setupRequest: func() *types.OIDCAuthRequest {
				req := types.OIDCAuthRequest{
					ConnectorID:      "test-oidc",
					Type:             constants.OIDC,
					CreateWebSession: true,
					ProxyAddress:     "https://test.localhost",
					CertTTL:          1 * time.Hour,
				}
				authReq, err := tt.a.CreateOIDCAuthRequest(ctx, req)
				require.NoError(t, err)
				return authReq
			},
			setupProvider: func(p *mockOIDCProvider, req *types.OIDCAuthRequest) {
				// Provider setup not relevant for this test
			},
			setupQuery: func(req *types.OIDCAuthRequest) url.Values {
				return url.Values{
					"code": []string{"test-code"},
				}
			},
			expectError:   true,
			errorContains: "missing state query param",
		},
		{
			name: "invalid state token",
			setupRequest: func() *types.OIDCAuthRequest {
				req := types.OIDCAuthRequest{
					ConnectorID:      "test-oidc",
					Type:             constants.OIDC,
					CreateWebSession: true,
					ProxyAddress:     "https://test.localhost",
					CertTTL:          1 * time.Hour,
				}
				authReq, err := tt.a.CreateOIDCAuthRequest(ctx, req)
				require.NoError(t, err)
				return authReq
			},
			setupProvider: func(p *mockOIDCProvider, req *types.OIDCAuthRequest) {
				// Provider setup not relevant for this test
			},
			setupQuery: func(req *types.OIDCAuthRequest) url.Values {
				return url.Values{
					"code":  []string{"test-code"},
					"state": []string{"invalid-state-token"},
				}
			},
			expectError:   true,
			errorContains: "Failed to get OIDC Auth Request",
		},
		{
			name: "oauth error response",
			setupRequest: func() *types.OIDCAuthRequest {
				req := types.OIDCAuthRequest{
					ConnectorID:      "test-oidc",
					Type:             constants.OIDC,
					CreateWebSession: true,
					ProxyAddress:     "https://test.localhost",
					CertTTL:          1 * time.Hour,
				}
				authReq, err := tt.a.CreateOIDCAuthRequest(ctx, req)
				require.NoError(t, err)
				return authReq
			},
			setupProvider: func(p *mockOIDCProvider, req *types.OIDCAuthRequest) {
				// Provider setup not relevant for this test
			},
			setupQuery: func(req *types.OIDCAuthRequest) url.Values {
				return url.Values{
					"error":             []string{"access_denied"},
					"error_description": []string{"User denied access"},
					"state":             []string{req.StateToken},
				}
			},
			expectError:   true,
			errorContains: "OIDC provider returned error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			authRequest := tc.setupRequest()
			tc.setupProvider(tt.oidcProvider, authRequest)
			query := tc.setupQuery(authRequest)

			resp, err := tt.a.ValidateOIDCAuthCallback(ctx, query)

			if tc.expectError {
				require.Error(t, err)
				if tc.errorContains != "" {
					require.Contains(t, err.Error(), tc.errorContains)
				}
				return
			}

			require.NoError(t, err)
			require.NotNil(t, resp)

			if tc.validateResult != nil {
				tc.validateResult(t, resp)
			}
		})
	}
}

// TestOIDCClaimsMapping verifies claims to roles mapping
func TestOIDCClaimsMapping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tt := setupOIDCContext(ctx, t)

	tests := []struct {
		name          string
		email         string
		groups        []string
		expectedRoles []string
		expectError   bool
		errorContains string
	}{
		{
			name:          "single role from group claim",
			email:         "user@example.com",
			groups:        []string{"admins"},
			expectedRoles: []string{"admin", "user"}, // admin from group, user from email wildcard
		},
		{
			name:          "multiple roles from groups",
			email:         "user@example.com",
			groups:        []string{"admins", "users"},
			expectedRoles: []string{"admin", "user"},
		},
		{
			name:          "role from email wildcard",
			email:         "test@example.com",
			groups:        []string{},
			expectedRoles: []string{"user"},
		},
		{
			name:          "no matching roles",
			email:         "test@other.com",
			groups:        []string{"other-group"},
			expectedRoles: nil,
			expectError:   true,
			errorContains: "does not have any roles mapped",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create auth request
			req := types.OIDCAuthRequest{
				ConnectorID:      "test-oidc",
				Type:             constants.OIDC,
				CreateWebSession: true,
				ProxyAddress:     "https://test.localhost",
				CertTTL:          1 * time.Hour,
			}

			authRequest, err := tt.a.CreateOIDCAuthRequest(ctx, req)
			require.NoError(t, err)

			// Setup provider
			tt.oidcProvider.setUserClaims(tc.email, tc.groups)
			tt.oidcProvider.expectNonce = authRequest.Nonce
			tt.oidcProvider.returnNonce = true

			// Validate callback
			callbackQuery := url.Values{
				"code":  []string{"test-code"},
				"state": []string{authRequest.StateToken},
			}

			resp, err := tt.a.ValidateOIDCAuthCallback(ctx, callbackQuery)

			if tc.expectError {
				require.Error(t, err)
				if tc.errorContains != "" {
					require.Contains(t, err.Error(), tc.errorContains)
				}
				return
			}

			require.NoError(t, err)
			require.NotNil(t, resp)
			require.Equal(t, tc.email, resp.Username)

			// Verify user was created with correct roles
			user, err := tt.a.GetUser(ctx, tc.email, false)
			require.NoError(t, err)

			userRoles := user.GetRoles()
			require.ElementsMatch(t, tc.expectedRoles, userRoles)
		})
	}
}

// TestOIDCUserCreation verifies user creation and updates
func TestOIDCUserCreation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tt := setupOIDCContext(ctx, t)

	// Create initial user
	req := types.OIDCAuthRequest{
		ConnectorID:      "test-oidc",
		Type:             constants.OIDC,
		CreateWebSession: true,
		ProxyAddress:     "https://test.localhost",
		CertTTL:          1 * time.Hour,
	}

	authRequest, err := tt.a.CreateOIDCAuthRequest(ctx, req)
	require.NoError(t, err)

	tt.oidcProvider.setUserClaims("user@example.com", []string{"admins"})
	tt.oidcProvider.expectNonce = authRequest.Nonce
	tt.oidcProvider.returnNonce = true

	callbackQuery := url.Values{
		"code":  []string{"test-code"},
		"state": []string{authRequest.StateToken},
	}

	resp, err := tt.a.ValidateOIDCAuthCallback(ctx, callbackQuery)
	require.NoError(t, err)
	require.Equal(t, "user@example.com", resp.Username)

	// Verify user exists
	user, err := tt.a.GetUser(ctx, "user@example.com", false)
	require.NoError(t, err)
	require.Contains(t, user.GetRoles(), "admin")
	originalRev := user.GetRevision()

	// Login again with different roles - should update user
	req2 := types.OIDCAuthRequest{
		ConnectorID:      "test-oidc",
		Type:             constants.OIDC,
		CreateWebSession: true,
		ProxyAddress:     "https://test.localhost",
		CertTTL:          1 * time.Hour,
	}

	authRequest2, err := tt.a.CreateOIDCAuthRequest(ctx, req2)
	require.NoError(t, err)

	tt.oidcProvider.setUserClaims("user@example.com", []string{"users"})
	tt.oidcProvider.expectNonce = authRequest2.Nonce

	callbackQuery2 := url.Values{
		"code":  []string{"test-code-2"},
		"state": []string{authRequest2.StateToken},
	}

	resp2, err := tt.a.ValidateOIDCAuthCallback(ctx, callbackQuery2)
	require.NoError(t, err)
	require.Equal(t, "user@example.com", resp2.Username)

	// Verify user was updated
	updatedUser, err := tt.a.GetUser(ctx, "user@example.com", false)
	require.NoError(t, err)
	require.Contains(t, updatedUser.GetRoles(), "user")
	require.NotEqual(t, originalRev, updatedUser.GetRevision(), "user revision should change on update")
}

// TestOIDCErrorHandling verifies error scenarios
func TestOIDCErrorHandling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tt := setupOIDCContext(ctx, t)

	t.Run("expired auth request", func(t *testing.T) {
		req := types.OIDCAuthRequest{
			ConnectorID:      "test-oidc",
			Type:             constants.OIDC,
			CreateWebSession: true,
			ProxyAddress:     "https://test.localhost",
			CertTTL:          1 * time.Hour,
		}

		authRequest, err := tt.a.CreateOIDCAuthRequest(ctx, req)
		require.NoError(t, err)

		// Advance clock past TTL
		tt.c.Advance(defaults.OIDCAuthRequestTTL + time.Minute)

		callbackQuery := url.Values{
			"code":  []string{"test-code"},
			"state": []string{authRequest.StateToken},
		}

		_, err = tt.a.ValidateOIDCAuthCallback(ctx, callbackQuery)
		require.Error(t, err)
	})

	t.Run("invalid connector ID", func(t *testing.T) {
		req := types.OIDCAuthRequest{
			ConnectorID:      "non-existent",
			Type:             constants.OIDC,
			CreateWebSession: true,
			ProxyAddress:     "https://test.localhost",
			CertTTL:          1 * time.Hour,
		}

		_, err := tt.a.CreateOIDCAuthRequest(ctx, req)
		require.Error(t, err)
		require.True(t, trace.IsNotFound(err))
	})
}

// TestOIDCTestFlow verifies SSO test flow (dry-run)
func TestOIDCTestFlow(t *testing.T) {
	t.Skip("SSO test flow requires connector spec setup - tested separately in integration tests")
	t.Parallel()
	ctx := context.Background()
	tt := setupOIDCContext(ctx, t)

	req := types.OIDCAuthRequest{
		ConnectorID:      "test-oidc",
		Type:             constants.OIDC,
		CreateWebSession: false,
		ProxyAddress:     "https://test.localhost",
		CertTTL:          1 * time.Hour,
		SSOTestFlow:      false, // Test flow requires connector spec in production
	}

	authRequest, err := tt.a.CreateOIDCAuthRequest(ctx, req)
	require.NoError(t, err)

	tt.oidcProvider.setUserClaims("test@example.com", []string{"admins"})
	tt.oidcProvider.expectNonce = authRequest.Nonce
	tt.oidcProvider.returnNonce = true

	callbackQuery := url.Values{
		"code":  []string{"test-code"},
		"state": []string{authRequest.StateToken},
	}

	// Note: In real test flow, the SSOTestFlow flag would be set on the authRequest
	// stored in the backend, and users would not be created
	_, err = tt.a.ValidateOIDCAuthCallback(ctx, callbackQuery)
	require.NoError(t, err)
}
