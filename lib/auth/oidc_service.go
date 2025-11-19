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

package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gravitational/trace"
	"golang.org/x/oauth2"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/constants"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/api/utils/keys/hardwarekey"
	"github.com/gravitational/teleport/lib/auth/authclient"
	"github.com/gravitational/teleport/lib/authz"
	"github.com/gravitational/teleport/lib/client/sso"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/loginrule"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"
)

// ErrOIDCNoRoles results from an OIDC user not having any roles mapped.
var ErrOIDCNoRoles = trace.AccessDenied("user does not have any roles mapped; check the claims_to_roles configuration in the OIDC connector")

const (
	// oidcProviderCacheTTL is the duration to cache OIDC provider configurations.
	// Discovery documents rarely change, so we cache them for 1 hour to reduce
	// network calls and prevent resource leaks from creating multiple HTTP clients.
	oidcProviderCacheTTL = 1 * time.Hour

	// oidcHTTPClientTimeout is the timeout for HTTP requests to OIDC providers.
	oidcHTTPClientTimeout = 30 * time.Second
)

// cachedOIDCProvider wraps an OIDC provider with its expiration time.
type cachedOIDCProvider struct {
	provider  *oidc.Provider
	expiresAt time.Time
}

// oidcAuthServiceImpl implements OIDCService interface
type oidcAuthServiceImpl struct {
	authServer    *Server
	httpClient    *http.Client
	providerCache sync.Map // map[string]*cachedOIDCProvider
	cacheTTL      time.Duration
}

// NewOIDCAuthService creates a new OIDC authentication service with proper
// HTTP client lifecycle management and provider caching to prevent resource leaks.
func NewOIDCAuthService(authServer *Server) OIDCService {
	return &oidcAuthServiceImpl{
		authServer: authServer,
		httpClient: &http.Client{
			Timeout: oidcHTTPClientTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
		cacheTTL: oidcProviderCacheTTL,
	}
}

// getOIDCProvider retrieves or creates an OIDC provider for the given issuer URL.
// It caches providers to avoid creating multiple HTTP clients and reduce network calls
// to the OIDC discovery endpoint.
func (s *oidcAuthServiceImpl) getOIDCProvider(ctx context.Context, issuerURL string) (*oidc.Provider, error) {
	// Check cache first
	if cached, ok := s.providerCache.Load(issuerURL); ok {
		cp := cached.(*cachedOIDCProvider)
		if time.Now().Before(cp.expiresAt) {
			return cp.provider, nil
		}
		// Cached provider expired, remove it
		s.providerCache.Delete(issuerURL)
	}

	// Create context with our HTTP client
	ctx = oidc.ClientContext(ctx, s.httpClient)

	// Add timeout for provider creation
	providerCtx, cancel := context.WithTimeout(ctx, oidcHTTPClientTimeout)
	defer cancel()

	// Create new provider with HTTP client context
	provider, err := oidc.NewProvider(providerCtx, issuerURL)
	if err != nil {
		return nil, trace.Wrap(err, "failed to create OIDC provider for issuer %q", issuerURL)
	}

	// Cache the provider
	s.providerCache.Store(issuerURL, &cachedOIDCProvider{
		provider:  provider,
		expiresAt: time.Now().Add(s.cacheTTL),
	})

	return provider, nil
}

// CreateOIDCAuthRequest creates an OIDC authentication request
func (s *oidcAuthServiceImpl) CreateOIDCAuthRequest(ctx context.Context, req types.OIDCAuthRequest) (*types.OIDCAuthRequest, error) {
	return s.createOIDCAuthRequest(ctx, req, false)
}

// CreateOIDCAuthRequestForMFA creates an OIDC authentication request for MFA
func (s *oidcAuthServiceImpl) CreateOIDCAuthRequestForMFA(ctx context.Context, req types.OIDCAuthRequest) (*types.OIDCAuthRequest, error) {
	return s.createOIDCAuthRequest(ctx, req, true)
}

// createOIDCAuthRequest is the common implementation for creating OIDC auth requests
func (s *oidcAuthServiceImpl) createOIDCAuthRequest(ctx context.Context, req types.OIDCAuthRequest, forMFA bool) (*types.OIDCAuthRequest, error) {
	// Get the OIDC connector
	connector, err := s.authServer.GetOIDCConnector(ctx, req.ConnectorID, true)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Validate client redirect URL for non-web-session flows
	// Requests for a web session originate from the proxy, so they are trusted
	// and handled in a way that minimizes misuse in the callback endpoint.
	// Requests for a client session (as used by tsh login) need to be checked,
	// as they will point the browser away from the IdP or the web UI after
	// authentication is done.
	if !req.CreateWebSession {
		ceremonyType := sso.CeremonyTypeLogin
		if req.SSOTestFlow {
			ceremonyType = sso.CeremonyTypeTest
		}

		if err := sso.ValidateClientRedirect(req.ClientRedirectURL, ceremonyType, connector.GetClientRedirectSettings()); err != nil {
			return nil, trace.Wrap(err, InvalidClientRedirectErrorMessage)
		}
	}

	// Generate state token for CSRF protection
	stateToken, err := utils.CryptoRandomHex(defaults.TokenLenBytes)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Generate nonce for ID token replay protection
	// The nonce is a random value that will be included in the ID token by the OIDC provider
	// and must be validated during the callback to prevent token replay attacks
	nonce, err := utils.CryptoRandomHex(defaults.TokenLenBytes)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Determine redirect URL
	redirectURL, err := services.GetRedirectURL(connector, req.ProxyAddress)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Get OIDC provider (cached) for endpoint discovery
	provider, err := s.getOIDCProvider(ctx, connector.GetIssuerURL())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Create OAuth2 config with discovered endpoints
	oauth2Config := oauth2.Config{
		ClientID:     connector.GetClientID(),
		ClientSecret: connector.GetClientSecret(),
		RedirectURL:  redirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       connector.GetScope(),
	}

	// Build auth code URL options
	var authURLOpts []oauth2.AuthCodeOption

	// Add PKCE if verifier is provided
	if req.PkceVerifier != "" {
		authURLOpts = append(authURLOpts, oauth2.S256ChallengeOption(req.PkceVerifier))
	}

	// Add nonce parameter for ID token replay protection (OIDC security best practice)
	authURLOpts = append(authURLOpts, oauth2.SetAuthURLParam("nonce", nonce))

	// Generate authorization URL
	authURL := oauth2Config.AuthCodeURL(stateToken, authURLOpts...)

	s.authServer.logger.DebugContext(ctx, "Creating OIDC auth request", "redirect_url", authURL)

	// Build auth request
	authRequest := types.OIDCAuthRequest{
		ConnectorID:       req.ConnectorID,
		Type:              req.Type,
		CheckUser:         req.CheckUser,
		StateToken:        stateToken,
		CSRFToken:         req.CSRFToken,
		RedirectURL:       authURL,
		ClientRedirectURL: req.ClientRedirectURL,
		CertTTL:           req.CertTTL,
		CreateWebSession:  req.CreateWebSession,
		ProxyAddress:      req.ProxyAddress,
		PkceVerifier:      req.PkceVerifier,
		Nonce:             nonce,
		SshPublicKey:      req.SshPublicKey,
		TlsPublicKey:      req.TlsPublicKey,
		SSOTestFlow:       req.SSOTestFlow,
	}

	// Store the request in backend storage with TTL
	// Note: Unlike GithubAuthRequest, OIDCAuthRequest doesn't have an Expires field,
	// so the TTL is managed by the backend storage layer via the ttl parameter
	if err := s.authServer.Services.CreateOIDCAuthRequest(ctx, authRequest, defaults.OIDCAuthRequestTTL); err != nil {
		return nil, trace.Wrap(err)
	}

	return &authRequest, nil
}

// ValidateOIDCAuthCallback validates the OIDC callback and creates a user session
func (s *oidcAuthServiceImpl) ValidateOIDCAuthCallback(ctx context.Context, q url.Values) (*authclient.OIDCAuthResponse, error) {
	diagCtx := NewSSODiagContext(types.KindOIDC, s.authServer)
	return validateOIDCAuthCallbackHelper(ctx, s, diagCtx, q, s.authServer.emitter, s.authServer.logger)
}

type oidcManager interface {
	ValidateOIDCAuthRedirect(ctx context.Context, diagCtx *SSODiagContext, q url.Values) (*authclient.OIDCAuthResponse, error)
}

// validateOIDCAuthCallbackHelper wraps the OIDC callback validation with audit logging and SSO diagnostics
func validateOIDCAuthCallbackHelper(ctx context.Context, m oidcManager, diagCtx *SSODiagContext, q url.Values, emitter apievents.Emitter, logger *slog.Logger) (*authclient.OIDCAuthResponse, error) {
	event := &apievents.UserLogin{
		Metadata: apievents.Metadata{
			Type: events.UserLoginEvent,
		},
		Method:             events.LoginMethodOIDC,
		ConnectionMetadata: authz.ConnectionMetadata(ctx),
	}

	auth, err := m.ValidateOIDCAuthRedirect(ctx, diagCtx, q)
	diagCtx.Info.Error = trace.UserMessage(err)
	event.AppliedLoginRules = diagCtx.Info.AppliedLoginRules

	// Write diagnostic info to backend
	diagCtx.WriteToBackend(ctx)

	claims := diagCtx.Info.OIDCClaims
	if claims != nil {
		// Convert OIDCClaims (map[string]interface{}) to map[string][]string for encoding
		stringMapClaims := make(map[string][]string)
		for k, v := range claims {
			switch val := v.(type) {
			case string:
				stringMapClaims[k] = []string{val}
			case []interface{}:
				var strVals []string
				for _, item := range val {
					if str, ok := item.(string); ok {
						strVals = append(strVals, str)
					}
				}
				if len(strVals) > 0 {
					stringMapClaims[k] = strVals
				}
			}
		}
		attributes, err := apievents.EncodeMapStrings(stringMapClaims)
		if err != nil {
			event.Status.UserMessage = "Failed to encode identity attributes"
			logger.DebugContext(ctx, "Failed to encode identity attributes", "error", err)
		} else {
			event.IdentityAttributes = attributes
		}
	}

	if err != nil {
		event.Code = events.UserSSOLoginFailureCode
		if diagCtx.Info.TestFlow {
			event.Code = events.UserSSOTestFlowLoginFailureCode
		}
		event.Status.Success = false
		event.Status.Error = trace.Unwrap(err).Error()
		event.Status.UserMessage = err.Error()

		if err := emitter.EmitAuditEvent(ctx, event); err != nil {
			logger.WarnContext(ctx, "Failed to emit OIDC login failed event", "error", err)
		}
		return nil, trace.Wrap(err)
	}

	event.Code = events.UserSSOLoginCode
	if diagCtx.Info.TestFlow {
		event.Code = events.UserSSOTestFlowLoginCode
	}
	event.Status.Success = true
	event.User = auth.Username

	if err := emitter.EmitAuditEvent(ctx, event); err != nil {
		logger.WarnContext(ctx, "Failed to emit OIDC login event", "error", err)
	}

	return auth, nil
}

// oidcCallbackParams holds validated OIDC callback parameters extracted from the redirect.
type oidcCallbackParams struct {
	code        string
	state       string
	authRequest *types.OIDCAuthRequest
	connector   types.OIDCConnector
	redirectURL string
}

// oidcTokenResult holds the result of OIDC token exchange.
type oidcTokenResult struct {
	token      *oauth2.Token
	rawIDToken string
}

// oidcClaimsResult holds verified claims extracted from the OIDC ID token.
type oidcClaimsResult struct {
	claims   map[string]interface{}
	username string
	roles    []string
}

// ValidateOIDCAuthRedirect validates OIDC auth callback redirect.
// This function orchestrates the OIDC authentication flow by coordinating
// multiple smaller operations: parameter extraction, token exchange, claims
// verification, user authentication, and response building.
func (s *oidcAuthServiceImpl) ValidateOIDCAuthRedirect(ctx context.Context, diagCtx *SSODiagContext, q url.Values) (*authclient.OIDCAuthResponse, error) {
	logger := s.authServer.logger.With(teleport.ComponentKey, "oidc")

	// Extract and validate callback parameters
	params, err := s.extractCallbackParams(ctx, diagCtx, logger, q)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Exchange authorization code for tokens
	tokenRes, err := s.exchangeOIDCToken(ctx, logger, params)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Verify ID token and extract claims
	claimsRes, err := s.verifyAndExtractClaims(ctx, logger, params, tokenRes)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Validate nonce to prevent token replay attacks
	if err := s.validateOIDCNonce(ctx, logger, params.authRequest, claimsRes.claims); err != nil {
		return nil, trace.Wrap(err)
	}

	// Store claims in diagnostic context for audit logging
	diagCtx.Info.OIDCClaims = claimsRes.claims

	// Authenticate user and create/update if needed
	userState, createParams, err := s.authenticateOIDCUser(ctx, diagCtx, logger, params, claimsRes)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Build auth response with sessions and certificates
	response, err := s.buildOIDCAuthResponse(ctx, logger, params, claimsRes, userState, createParams)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return response, nil
}

// extractCallbackParams extracts and validates OIDC callback parameters.
// It handles OAuth2 error responses, validates required parameters (code, state),
// retrieves the stored auth request, and loads the OIDC connector configuration.
func (s *oidcAuthServiceImpl) extractCallbackParams(ctx context.Context, diagCtx *SSODiagContext, logger *slog.Logger, q url.Values) (*oidcCallbackParams, error) {
	// Check for OAuth2 error response from provider
	if errParam := q.Get("error"); errParam != "" {
		// Try to find request so the error gets logged against it
		state := q.Get("state")
		if state != "" {
			diagCtx.RequestID = state
			req, err := s.authServer.Services.GetOIDCAuthRequest(ctx, state)
			if err == nil {
				diagCtx.Info.TestFlow = req.SSOTestFlow
			}
		}

		// Optional parameter: error_description
		errDesc := q.Get("error_description")
		oauthErr := trace.OAuth2("invalid_request", errParam, q)
		return nil, trace.WithUserMessage(oauthErr, "OIDC provider returned error: %v [%v]", errDesc, errParam)
	}

	// Extract and validate authorization code
	code := q.Get("code")
	if code == "" {
		oauthErr := trace.OAuth2("invalid_request", "code query param must be set", q)
		return nil, trace.WithUserMessage(oauthErr, "Invalid parameters received from OIDC provider.")
	}

	// Extract and validate state token for CSRF protection
	state := q.Get("state")
	if state == "" {
		oauthErr := trace.OAuth2("invalid_request", "missing state query param", q)
		return nil, trace.WithUserMessage(oauthErr, "Invalid parameters received from OIDC provider.")
	}
	diagCtx.RequestID = state

	// Retrieve stored auth request from backend storage
	authRequest, err := s.authServer.Services.GetOIDCAuthRequest(ctx, state)
	if err != nil {
		return nil, trace.Wrap(err, "Failed to get OIDC Auth Request.")
	}
	diagCtx.Info.TestFlow = authRequest.SSOTestFlow

	logger.InfoContext(ctx, "Retrieved auth request",
		"connector_id", authRequest.ConnectorID,
		"proxy_address", authRequest.ProxyAddress,
		"create_web_session", authRequest.CreateWebSession,
		"ssh_pub_key_len", len(authRequest.SshPublicKey),
		"tls_pub_key_len", len(authRequest.TlsPublicKey))

	// Get the OIDC connector configuration
	connector, err := s.authServer.GetOIDCConnector(ctx, authRequest.ConnectorID, true)
	if err != nil {
		return nil, trace.Wrap(err, "Failed to get OIDC connector.")
	}

	// Determine redirect URL for token exchange
	redirectURL, err := services.GetRedirectURL(connector, authRequest.ProxyAddress)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	logger.InfoContext(ctx, "Validating OIDC callback",
		"code_length", len(code),
		"state_length", len(state),
		"redirect_url", redirectURL,
		"proxy_address", authRequest.ProxyAddress)

	return &oidcCallbackParams{
		code:        code,
		state:       state,
		authRequest: authRequest,
		connector:   connector,
		redirectURL: redirectURL,
	}, nil
}

// exchangeOIDCToken exchanges the authorization code for OAuth2 tokens.
// It creates an OAuth2 config with the OIDC provider endpoints, adds PKCE
// verification if needed, and exchanges the code for an access token and ID token.
func (s *oidcAuthServiceImpl) exchangeOIDCToken(ctx context.Context, logger *slog.Logger, params *oidcCallbackParams) (*oidcTokenResult, error) {
	// Get OIDC provider (cached) for endpoint discovery
	provider, err := s.getOIDCProvider(ctx, params.connector.GetIssuerURL())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Create OAuth2 config with discovered endpoints
	oauth2Config := oauth2.Config{
		ClientID:     params.connector.GetClientID(),
		ClientSecret: params.connector.GetClientSecret(),
		RedirectURL:  params.redirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       params.connector.GetScope(),
	}

	// Debug logging (without exposing client secret)
	logger.InfoContext(ctx, "OIDC token exchange",
		"client_id", params.connector.GetClientID(),
		"client_secret_length", len(params.connector.GetClientSecret()),
		"redirect_url", params.redirectURL,
		"token_url", oauth2Config.Endpoint.TokenURL,
		"issuer_url", params.connector.GetIssuerURL())

	// Prepare token exchange options with PKCE verifier if provided
	var tokenOpts []oauth2.AuthCodeOption
	if params.authRequest.PkceVerifier != "" {
		tokenOpts = append(tokenOpts, oauth2.VerifierOption(params.authRequest.PkceVerifier))
	}

	// Add explicit timeout for token exchange to prevent indefinite hangs
	exchangeCtx, exchangeCancel := context.WithTimeout(ctx, oidcHTTPClientTimeout)
	defer exchangeCancel()

	// Exchange authorization code for tokens
	token, err := oauth2Config.Exchange(exchangeCtx, params.code, tokenOpts...)
	if err != nil {
		logger.ErrorContext(ctx, "Token exchange failed",
			"error", err,
			"client_id", params.connector.GetClientID(),
			"redirect_url", params.redirectURL)
		return nil, trace.Wrap(err, "failed to exchange code for token")
	}

	// Extract ID token from token response
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		return nil, trace.BadParameter("no id_token in token response")
	}

	return &oidcTokenResult{
		token:      token,
		rawIDToken: rawIDToken,
	}, nil
}

// verifyAndExtractClaims verifies the ID token signature and extracts claims.
// It uses the OIDC provider's JWKS keys to verify the token signature,
// extracts claims from the verified token, and maps claims to username and roles.
func (s *oidcAuthServiceImpl) verifyAndExtractClaims(ctx context.Context, logger *slog.Logger, params *oidcCallbackParams, tokenRes *oidcTokenResult) (*oidcClaimsResult, error) {
	// Get OIDC provider for token verification
	provider, err := s.getOIDCProvider(ctx, params.connector.GetIssuerURL())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Create ID token verifier with client ID validation
	verifier := provider.Verifier(&oidc.Config{
		ClientID: params.connector.GetClientID(),
	})

	// Add explicit timeout for ID token verification (may fetch JWKS keys)
	verifyCtx, verifyCancel := context.WithTimeout(ctx, oidcHTTPClientTimeout)
	defer verifyCancel()

	// Verify ID token signature and claims
	idToken, err := verifier.Verify(verifyCtx, tokenRes.rawIDToken)
	if err != nil {
		return nil, trace.Wrap(err, "failed to verify ID token")
	}

	// Extract claims from verified ID token
	var claims map[string]interface{}
	if err := idToken.Claims(&claims); err != nil {
		return nil, trace.Wrap(err, "failed to extract claims")
	}

	// Determine username from claims based on connector configuration
	username, err := s.getUsernameFromClaims(params.connector, claims)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	logger.DebugContext(ctx, "Retrieved OIDC claims",
		"username", username,
		"num_claims", len(claims))

	// Map claims to Teleport roles
	roles := s.mapClaimsToRoles(params.connector, claims)
	if len(roles) == 0 {
		// Use generic error message - don't reveal username
		return nil, trace.Wrap(ErrOIDCNoRoles)
	}

	return &oidcClaimsResult{
		claims:   claims,
		username: username,
		roles:    roles,
	}, nil
}

// validateOIDCNonce validates the nonce claim in the ID token to prevent replay attacks.
// The nonce in the ID token must match the nonce sent in the authorization request.
// This is a critical security check to prevent token replay attacks.
func (s *oidcAuthServiceImpl) validateOIDCNonce(ctx context.Context, logger *slog.Logger, authRequest *types.OIDCAuthRequest, claims map[string]interface{}) error {
	// Nonce validation is only required if nonce was sent in the auth request
	if authRequest.Nonce == "" {
		return nil
	}

	// Extract nonce from claims
	claimNonce, ok := claims["nonce"].(string)
	if !ok {
		return trace.BadParameter("ID token missing nonce claim")
	}

	// Verify nonce matches the one sent in the auth request
	if claimNonce != authRequest.Nonce {
		logger.WarnContext(ctx, "Nonce validation failed",
			"expected", authRequest.Nonce,
			"got", claimNonce)
		return trace.AccessDenied("nonce mismatch: potential token replay attack detected")
	}

	logger.DebugContext(ctx, "Nonce validation successful")
	return nil
}

// authenticateOIDCUser handles user authentication, role mapping, and user creation/update.
// It calculates user attributes, applies login rules, creates or updates the user,
// calls login hooks, and retrieves the final user state.
func (s *oidcAuthServiceImpl) authenticateOIDCUser(ctx context.Context, diagCtx *SSODiagContext, logger *slog.Logger, params *oidcCallbackParams, claimsRes *oidcClaimsResult) (services.UserState, *CreateOIDCUserParams, error) {
	// Calculate user attributes and apply login rules
	createParams, err := s.calculateOIDCUser(ctx, diagCtx, params.connector, claimsRes.username, claimsRes.roles, claimsRes.claims, params.authRequest)
	if err != nil {
		// Use generic error message
		return nil, nil, trace.Wrap(err, "Failed to calculate user attributes.")
	}

	// Store user creation params in diagnostic context for audit logging
	diagCtx.Info.CreateUserParams = &types.CreateUserParams{
		ConnectorName: createParams.ConnectorName,
		Username:      createParams.Username,
		KubeGroups:    createParams.KubeGroups,
		KubeUsers:     createParams.KubeUsers,
		Roles:         createParams.Roles,
		Traits:        createParams.Traits,
		SessionTTL:    types.Duration(createParams.SessionTTL),
	}

	// Create or update user in the backend
	user, err := s.createOIDCUser(ctx, createParams, params.authRequest.SSOTestFlow)
	if err != nil {
		return nil, nil, trace.Wrap(err, "Failed to create user from provided parameters.")
	}

	// Call login hooks for custom authentication logic
	if err := s.authServer.CallLoginHooks(ctx, user); err != nil {
		return nil, nil, trace.Wrap(err)
	}

	// Get final user state including any modifications from login hooks
	userState, err := s.authServer.GetUserOrLoginState(ctx, user.GetName())
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}

	return userState, createParams, nil
}

// buildOIDCAuthResponse builds the final OIDC authentication response.
// It creates web sessions and/or SSH/TLS certificates based on the request type,
// and handles both normal authentication flow and SSO test flow.
func (s *oidcAuthServiceImpl) buildOIDCAuthResponse(ctx context.Context, logger *slog.Logger, params *oidcCallbackParams, claimsRes *oidcClaimsResult, userState services.UserState, createParams *CreateOIDCUserParams) (*authclient.OIDCAuthResponse, error) {
	// In test flow, skip signing and creating web sessions
	if params.authRequest.SSOTestFlow {
		return &authclient.OIDCAuthResponse{
			Req: authclient.OIDCAuthRequest{
				ConnectorID:       params.authRequest.ConnectorID,
				CSRFToken:         params.authRequest.CSRFToken,
				CreateWebSession:  params.authRequest.CreateWebSession,
				ClientRedirectURL: params.authRequest.ClientRedirectURL,
				SSHPubKey:         params.authRequest.SshPublicKey,
				TLSPubKey:         params.authRequest.TlsPublicKey,
			},
			Identity: types.ExternalIdentity{
				ConnectorID: params.connector.GetName(),
				Username:    claimsRes.username,
			},
			Username: createParams.Username,
		}, nil
	}

	// For normal authentication, create sessions and certificates
	response, err := s.makeOIDCAuthResponse(ctx, params.authRequest, userState, params.connector.GetName(), claimsRes.username, createParams.SessionTTL)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return response, nil
}

// getUsernameFromClaims extracts the username from OIDC claims based on connector configuration
func (s *oidcAuthServiceImpl) getUsernameFromClaims(connector types.OIDCConnector, claims map[string]interface{}) (string, error) {
	usernameClaim := connector.GetUsernameClaim()
	if usernameClaim == "" {
		usernameClaim = "email"
	}

	username, ok := claims[usernameClaim].(string)
	if !ok || username == "" {
		return "", trace.BadParameter("username claim %q not found or empty in OIDC claims", usernameClaim)
	}

	return username, nil
}

// mapClaimsToRoles maps OIDC claims to Teleport roles
func (s *oidcAuthServiceImpl) mapClaimsToRoles(connector types.OIDCConnector, claims map[string]interface{}) []string {
	var roles []string
	rolesMap := make(map[string]bool)

	for _, mapping := range connector.GetClaimsToRoles() {
		var matched bool

		claimValue, exists := claims[mapping.Claim]
		if !exists {
			continue
		}

		// Handle both string and array claim values
		switch v := claimValue.(type) {
		case string:
			matched = matchPattern(mapping.Value, v)
		case []interface{}:
			for _, item := range v {
				if str, ok := item.(string); ok {
					if matchPattern(mapping.Value, str) {
						matched = true
						break
					}
				}
			}
		}

		if matched {
			for _, role := range mapping.Roles {
				if !rolesMap[role] {
					rolesMap[role] = true
					roles = append(roles, role)
				}
			}
		}
	}

	return roles
}

// matchPattern matches a value against a pattern (supports wildcards)
func matchPattern(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	if len(pattern) > 0 && pattern[0] == '*' {
		suffix := pattern[1:]
		return len(value) >= len(suffix) && value[len(value)-len(suffix):] == suffix
	}
	return pattern == value
}

// CreateOIDCUserParams is a set of parameters used to create an OIDC user
type CreateOIDCUserParams struct {
	// ConnectorName is the name of the connector
	ConnectorName string

	// Username is the Teleport user name
	Username string

	// KubeGroups is the list of Kubernetes groups
	KubeGroups []string

	// KubeUsers is the list of Kubernetes users
	KubeUsers []string

	// Roles is the list of roles
	Roles []string

	// Traits is the list of traits
	Traits map[string][]string

	// SessionTTL is the session TTL
	SessionTTL time.Duration
}

// calculateOIDCUser calculates user parameters and applies login rules
func (s *oidcAuthServiceImpl) calculateOIDCUser(ctx context.Context, diagCtx *SSODiagContext, connector types.OIDCConnector, username string, roles []string, claims map[string]interface{}, request *types.OIDCAuthRequest) (*CreateOIDCUserParams, error) {
	p := CreateOIDCUserParams{
		ConnectorName: connector.GetName(),
		Username:      username,
		Roles:         roles,
	}

	// Extract traits from claims
	p.Traits = make(map[string][]string)
	p.Traits[constants.TraitLogins] = []string{username}

	// Extract groups from claims if available
	if groupsClaim, ok := claims["groups"].([]interface{}); ok {
		var groups []string
		for _, g := range groupsClaim {
			if str, ok := g.(string); ok {
				groups = append(groups, str)
			}
		}
		if len(groups) > 0 {
			p.Traits[constants.TraitKubeGroups] = groups
			p.KubeGroups = groups
		}
	}

	// Apply login rules
	evaluationInput := &loginrule.EvaluationInput{
		Traits: p.Traits,
	}
	evaluationOutput, err := s.authServer.GetLoginRuleEvaluator().Evaluate(ctx, evaluationInput)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	p.Traits = evaluationOutput.Traits
	diagCtx.Info.AppliedLoginRules = evaluationOutput.AppliedRules

	// Update kube groups and users from traits after login rule evaluation
	p.KubeGroups = p.Traits[constants.TraitKubeGroups]
	p.KubeUsers = p.Traits[constants.TraitKubeUsers]

	// Calculate session TTL
	roleSet, err := services.FetchRoles(p.Roles, s.authServer, p.Traits)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	roleTTL := roleSet.AdjustSessionTTL(apidefaults.MaxCertDuration)
	p.SessionTTL = utils.MinTTL(roleTTL, request.CertTTL)

	return &p, nil
}

// createOIDCUser creates or updates an OIDC user
func (s *oidcAuthServiceImpl) createOIDCUser(ctx context.Context, p *CreateOIDCUserParams, dryRun bool) (types.User, error) {
	s.authServer.logger.DebugContext(ctx, "Generating dynamic OIDC identity",
		"connector_name", p.ConnectorName,
		"user_name", p.Username,
		"roles", p.Roles,
		"dry_run", dryRun,
	)

	expires := s.authServer.GetClock().Now().UTC().Add(p.SessionTTL)

	user := &types.UserV2{
		Kind:    types.KindUser,
		Version: types.V2,
		Metadata: types.Metadata{
			Name:      p.Username,
			Namespace: apidefaults.Namespace,
			Expires:   &expires,
		},
		Spec: types.UserSpecV2{
			Roles:  p.Roles,
			Traits: p.Traits,
			OIDCIdentities: []types.ExternalIdentity{{
				ConnectorID: p.ConnectorName,
				Username:    p.Username,
			}},
			CreatedBy: types.CreatedBy{
				User: types.UserRef{Name: teleport.UserSystem},
				Time: s.authServer.GetClock().Now().UTC(),
				Connector: &types.ConnectorRef{
					Type:     constants.OIDC,
					ID:       p.ConnectorName,
					Identity: p.Username,
				},
			},
		},
	}

	if dryRun {
		return user, nil
	}

	existingUser, err := s.authServer.Services.GetUser(ctx, p.Username, false)
	if err != nil && !trace.IsNotFound(err) {
		return nil, trace.Wrap(err)
	}

	if existingUser != nil {
		ref := user.GetCreatedBy().Connector
		if !ref.IsSameProvider(existingUser.GetCreatedBy().Connector) {
			return nil, trace.AlreadyExists("local user already exists and is not an OIDC user")
		}

		user.SetRevision(existingUser.GetRevision())
		if _, err := s.authServer.UpdateUser(ctx, user); err != nil {
			return nil, trace.Wrap(err)
		}
	} else {
		if _, err := s.authServer.CreateUser(ctx, user); err != nil {
			return nil, trace.Wrap(err)
		}
	}

	return user, nil
}

// makeOIDCAuthResponse creates the final OIDC auth response with sessions and certificates
func (s *oidcAuthServiceImpl) makeOIDCAuthResponse(
	ctx context.Context,
	req *types.OIDCAuthRequest,
	userState services.UserState,
	connectorName string,
	oidcUsername string,
	sessionTTL time.Duration) (*authclient.OIDCAuthResponse, error) {

	auth := authclient.OIDCAuthResponse{
		Req: authclient.OIDCAuthRequest{
			ConnectorID:       req.ConnectorID,
			CSRFToken:         req.CSRFToken,
			CreateWebSession:  req.CreateWebSession,
			ClientRedirectURL: req.ClientRedirectURL,
			SSHPubKey:         req.SshPublicKey,
			TLSPubKey:         req.TlsPublicKey,
		},
		Identity: types.ExternalIdentity{
			ConnectorID: connectorName,
			Username:    oidcUsername,
		},
		Username: userState.GetName(),
	}

	// If the request is coming from a browser, create a web session
	if req.CreateWebSession {
		session, err := s.authServer.CreateWebSessionFromReq(ctx, NewWebSessionRequest{
			User:                 userState.GetName(),
			Roles:                userState.GetRoles(),
			Traits:               userState.GetTraits(),
			SessionTTL:           sessionTTL,
			LoginTime:            s.authServer.clock.Now().UTC(),
			LoginIP:              req.ClientLoginIP,
			LoginUserAgent:       req.ClientUserAgent,
			AttestWebSession:     true,
			CreateDeviceWebToken: true,
		})
		if err != nil {
			return nil, trace.Wrap(err, "Failed to create web session.")
		}

		auth.Session = session
	}

	// If a public key was provided, sign it and return a certificate
	if len(req.SshPublicKey) != 0 || len(req.TlsPublicKey) != 0 {
		s.authServer.logger.InfoContext(ctx, "Generating certificates for console login",
			"ssh_pub_key_len", len(req.SshPublicKey),
			"tls_pub_key_len", len(req.TlsPublicKey),
			"username", userState.GetName())

		sshCert, tlsCert, err := s.authServer.CreateSessionCerts(ctx, &SessionCertsRequest{
			UserState:               userState,
			SessionTTL:              sessionTTL,
			SSHPubKey:               req.SshPublicKey,
			TLSPubKey:               req.TlsPublicKey,
			SSHAttestationStatement: hardwarekey.AttestationStatementFromProto(req.SshAttestationStatement),
			TLSAttestationStatement: hardwarekey.AttestationStatementFromProto(req.TlsAttestationStatement),
			Compatibility:           req.Compatibility,
			RouteToCluster:          req.RouteToCluster,
			KubernetesCluster:       req.KubernetesCluster,
			LoginIP:                 req.ClientLoginIP,
		})
		if err != nil {
			return nil, trace.Wrap(err, "Failed to create session certificate.")
		}

		clusterName, err := s.authServer.GetClusterName(ctx)
		if err != nil {
			return nil, trace.Wrap(err, "Failed to obtain cluster name.")
		}

		auth.Cert = sshCert
		auth.TLSCert = tlsCert

		// Return the host CA for this cluster only
		authority, err := s.authServer.GetCertAuthority(ctx, types.CertAuthID{
			Type:       types.HostCA,
			DomainName: clusterName.GetClusterName(),
		}, false)
		if err != nil {
			return nil, trace.Wrap(err, "Failed to obtain cluster's host CA.")
		}
		auth.HostSigners = append(auth.HostSigners, authority)
	}

	// Calculate client options for the user
	if o, err := s.authServer.ClientOptionsForLogin(userState); err == nil {
		auth.ClientOptions = o
	} else {
		logger := s.authServer.logger
		logger.WarnContext(ctx, "Failed to calculate client options for OIDC login", "username", userState.GetName(), "error", err)
	}

	return &auth, nil
}

// generateRandomToken generates a random token for CSRF protection
func generateRandomToken(length int) (string, error) {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return "", trace.Wrap(err)
	}
	return base64.URLEncoding.EncodeToString(b), nil
}
