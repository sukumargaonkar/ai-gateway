// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package rotators

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/oauth2"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
	"google.golang.org/api/sts/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	aigv1a1 "github.com/envoyproxy/ai-gateway/api/v1alpha1"
	"github.com/envoyproxy/ai-gateway/internal/controller/tokenprovider"
)

const (
	// gcpAccessTokenKey is the key used to store GCP access token in Kubernetes secrets.
	gcpAccessTokenKey = "gcpAccessToken"
	// grantTypeTokenExchange is the OAuth 2.0 grant type for token exchange.
	grantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange" //nolint:gosec
	// gcpIAMScope is the OAuth scope for IAM operations in GCP.
	gcpIAMScope = "https://www.googleapis.com/auth/iam" //nolint:gosec
	// tokenTypeAccessToken indicates the requested token type is an access token.
	tokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token" //nolint:gosec
	// tokenTypeJWT indicates the subject token type is a JWT.
	tokenTypeJWT = "urn:ietf:params:oauth:token-type:jwt" //nolint:gosec
	// stsTokenScope is the OAuth scope for GCP cloud platform operations.
	stsTokenScope = "https://www.googleapis.com/auth/cloud-platform" //nolint:gosec
)

// gcpOIDCTokenRotator implements Rotator interface for GCP access token exchange.
// It handles the complete authentication flow for GCP Workload Identity Federation:
// 1. Obtaining an OIDC token from the configured provider
// 2. Exchanging the OIDC token for a GCP STS token
// 3. Using the STS token to impersonate a GCP service account
// 4. Storing the resulting access token in a Kubernetes secret
type gcpOIDCTokenRotator struct {
	client client.Client        // Kubernetes client for interacting with the cluster
	kube   kubernetes.Interface // Kubernetes client interface
	logger logr.Logger          // Logger for recording rotator activities
	// GCP Credentials configuration from BackendSecurityPolicy
	gcpCredentials aigv1a1.BackendSecurityPolicyGCPCredentials
	// backendSecurityPolicyName provides name of backend security policy.
	backendSecurityPolicyName string
	// backendSecurityPolicyNamespace provides namespace of backend security policy.
	backendSecurityPolicyNamespace string
	// preRotationWindow is the duration before token expiry when rotation should occur
	preRotationWindow time.Duration
	// oidcProvider provides the OIDC token needed for GCP Workload Identity Federation
	oidcProvider tokenprovider.TokenProvider
}

// NewGCPOIDCTokenRotator creates a new gcpOIDCTokenRotator with the given parameters.
// This rotator handles the GCP Workload Identity Federation authentication flow
// by obtaining and refreshing GCP access tokens.
func NewGCPOIDCTokenRotator(
	ctx context.Context,
	client client.Client,
	kube kubernetes.Interface,
	logger logr.Logger,
	bsp *aigv1a1.BackendSecurityPolicy,
	preRotationWindow time.Duration,
) (Rotator, error) {
	logger = logger.WithName("gcp-token-rotator")

	if bsp == nil {
		return nil, fmt.Errorf("backend security policy cannot be nil")
	}

	if bsp.Spec.GCPCredentials == nil {
		return nil, fmt.Errorf("invalid backend security policy, gcp credentials cannot be nil")
	}

	// Get the OIDC configuration from the backend security policy
	oidcConfig := bsp.Spec.GCPCredentials.WorkLoadIdentityFederationConfig.WorkloadIdentityProvider.OIDCProvider.OIDC

	// Create the OIDC token provider that will be used to get tokens from the OIDC provider
	oidcProvider, err := tokenprovider.NewOidcTokenProvider(ctx, client, &oidcConfig)
	if err != nil {
		logger.Error(err, "failed to construct oidc provider")
		return nil, fmt.Errorf("failed to initialize OIDC provider: %w", err)
	}

	return &gcpOIDCTokenRotator{
		client:                         client,
		kube:                           kube,
		logger:                         logger,
		gcpCredentials:                 *bsp.Spec.GCPCredentials,
		backendSecurityPolicyName:      bsp.Name,
		backendSecurityPolicyNamespace: bsp.Namespace,
		preRotationWindow:              preRotationWindow,
		oidcProvider:                   oidcProvider,
	}, nil
}

// IsExpired implements Rotator.IsExpired method to check if the preRotation time is before the current time.
// This determines whether a token needs to be rotated based on its pre-rotation expiration time.
// A token is considered expired if the current time is after the pre-rotation expiration time.
func (r *gcpOIDCTokenRotator) IsExpired(preRotationExpirationTime time.Time) bool {
	// Use the common IsBufferedTimeExpired helper to determine if the token has expired
	// A buffer of 0 means we check exactly at the pre-rotation time
	return IsBufferedTimeExpired(0, preRotationExpirationTime)
}

// GetPreRotationTime implements Rotator.GetPreRotationTime method to retrieve the pre-rotation time for GCP token.
// This calculates when a token should be proactively rotated before it expires.
// The pre-rotation time is determined by subtracting the pre-rotation window from the token's expiration time.
func (r *gcpOIDCTokenRotator) GetPreRotationTime(ctx context.Context) (time.Time, error) {
	// Look up the secret containing the current token
	secret, err := LookupSecret(ctx, r.client, r.backendSecurityPolicyNamespace, GetBSPSecretName(r.backendSecurityPolicyName))
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the secret doesn't exist, return zero time to indicate immediate rotation is needed
			return time.Time{}, nil
		}
		return time.Time{}, fmt.Errorf("failed to lookup secret: %w", err)
	}
	// Extract the token expiration time from the secret's annotations
	expirationTime, err := GetExpirationSecretAnnotation(secret)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to get expiration time from secret: %w", err)
	}

	// Calculate the pre-rotation time by subtracting the pre-rotation window from the expiration time
	// This ensures tokens are rotated before they expire
	preRotationTime := expirationTime.Add(-r.preRotationWindow)
	return preRotationTime, nil
}

// Rotate implements Rotator.Rotate method to rotate GCP access token and updates the Kubernetes secret.
// The token rotation process follows these steps:
// 1. Obtain an OIDC token from the configured provider
// 2. Exchange the OIDC token for a GCP STS token
// 3. Use the STS token to impersonate the specified GCP service account
// 4. Store the resulting access token in a Kubernetes secret
// Returns the expiration time of the new token and any error encountered during rotation.
func (r *gcpOIDCTokenRotator) Rotate(ctx context.Context) (time.Time, error) {
	secretName := GetBSPSecretName(r.backendSecurityPolicyName)

	r.logger.Info("start rotating gcp access token", "namespace", r.backendSecurityPolicyNamespace, "name", r.backendSecurityPolicyName)

	// 1. Get OIDCProvider Token
	// This is the initial token from the configured OIDC provider (e.g., Kubernetes service account token)
	oidcTokenExpiry, err := r.oidcProvider.GetToken(ctx)
	if err != nil {
		r.logger.Error(err, "failed to get token from oidc provider", "oidcIssuer", r.gcpCredentials.WorkLoadIdentityFederationConfig.WorkloadIdentityProvider.Name)
		return time.Time{}, fmt.Errorf("failed to obtain OIDC token: %w", err)
	}

	// 2. Exchange the JWT for an STS token.
	// The OIDC JWT token is exchanged for a Google Cloud STS token
	stsToken, err := r.exchangeJWTForSTSToken(ctx, oidcTokenExpiry.Token)
	if err != nil {
		r.logger.Error(err, "failed to exchange JWT for STS token")
		return time.Time{}, fmt.Errorf("failed to exchange JWT for STS token: %w", err)
	}

	// 3. Exchange the STS token for a GCP service account access token.
	// The STS token is used to impersonate a GCP service account
	gcpAccessToken, err := r.impersonateServiceAccount(ctx, stsToken)
	if err != nil {
		r.logger.Error(err, "failed to exchange STS token for GCP access token")
		return time.Time{}, fmt.Errorf("failed to impersonate service account: %w", err)
	}
	gcpTokenExpiry := tokenprovider.TokenExpiry{Token: gcpAccessToken.AccessToken, ExpiresAt: gcpAccessToken.Expiry}

	secret, err := LookupSecret(ctx, r.client, r.backendSecurityPolicyNamespace, secretName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.logger.Info("creating a new gcp access token into secret", "namespace", r.backendSecurityPolicyNamespace, "name", r.backendSecurityPolicyName)
			secret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: r.backendSecurityPolicyNamespace,
				},
				Type: corev1.SecretTypeOpaque,
				Data: make(map[string][]byte),
			}
			populateAccessTokenInSecret(secret, &gcpTokenExpiry, gcpAccessTokenKey)
			err = r.client.Create(ctx, secret)
			if err != nil {
				r.logger.Error(err, "failed to create gcp access token", "namespace", r.backendSecurityPolicyNamespace, "name", r.backendSecurityPolicyName)
				return time.Time{}, err
			}
			return gcpTokenExpiry.ExpiresAt, nil
		}
		r.logger.Error(err, "failed to lookup gcp access token secret", "namespace", r.backendSecurityPolicyNamespace, "name", r.backendSecurityPolicyName)
		return time.Time{}, err
	}
	r.logger.Info("updating gcp access token secret", "namespace", r.backendSecurityPolicyNamespace, "name", r.backendSecurityPolicyName)

	populateAccessTokenInSecret(secret, &gcpTokenExpiry, gcpAccessTokenKey)
	err = r.client.Update(ctx, secret)
	if err != nil {
		r.logger.Error(err, "failed to update gcp access token", "namespace", r.backendSecurityPolicyNamespace, "name", r.backendSecurityPolicyName)
		return time.Time{}, err
	}
	return gcpTokenExpiry.ExpiresAt, nil
}

// exchangeJWTForSTSToken exchanges a signed JWT for a Google Cloud STS token.
// This is the first step in the GCP Workload Identity Federation flow:
// 1. The JWT token from an OIDC provider is exchanged for a short-lived STS token
// 2. The STS token can then be used to impersonate a GCP service account
//
// Parameters:
// - ctx: Context for the request
// - jwtToken: The JWT token from the OIDC provider
// - opts: Optional client options for the STS service
//
// Returns the STS access token or an error if the exchange fails.
func (r *gcpOIDCTokenRotator) exchangeJWTForSTSToken(ctx context.Context, jwtToken string, opts ...option.ClientOption) (string, error) {
	// Create an STS client.
	opts = append(opts, option.WithoutAuthentication())
	stsService, err := sts.NewService(ctx, opts...)
	if err != nil {
		return "", fmt.Errorf("error creating STS service: %w", err)
	}
	// Construct the STS request.
	// Build the audience string in the format required by GCP Workload Identity Federation
	wifConfig := r.gcpCredentials.WorkLoadIdentityFederationConfig
	stsAudience := fmt.Sprintf("//iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s/providers/%s",
		wifConfig.ProjectID,
		wifConfig.WorkloadIdentityPoolName,
		wifConfig.WorkloadIdentityProvider.Name)

	// Create the token exchange request with the appropriate parameters
	req := &sts.GoogleIdentityStsV1ExchangeTokenRequest{
		GrantType:          grantTypeTokenExchange,
		Audience:           stsAudience,
		Scope:              gcpIAMScope,
		RequestedTokenType: tokenTypeAccessToken,
		SubjectToken:       jwtToken,
		SubjectTokenType:   tokenTypeJWT,
	}

	// Call the STS API.
	resp, err := stsService.V1.Token(req).Do()
	if err != nil {
		return "", fmt.Errorf("error calling STS Token API: %w", err)
	}

	return resp.AccessToken, nil
}

// impersonateServiceAccount exchanges an STS token for a GCP service account access token using impersonation.
// This is the second step in the GCP Workload Identity Federation flow:
// 1. An STS token is used to impersonate a GCP service account
// 2. The resulting access token has the permissions of the impersonated service account
//
// Parameters:
// - ctx: Context for the request
// - stsToken: The STS token obtained from exchangeJWTForSTSToken
// - opts: Optional client options for the impersonation service
//
// Returns the GCP service account access token or an error if impersonation fails.
func (r *gcpOIDCTokenRotator) impersonateServiceAccount(ctx context.Context, stsToken string, opts ...option.ClientOption) (*oauth2.Token, error) {
	// Construct the service account email from the configured parameters
	saImpersonation := r.gcpCredentials.WorkLoadIdentityFederationConfig.ServiceAccountImpersonation
	saEmail := fmt.Sprintf("%s@%s.iam.gserviceaccount.com", saImpersonation.ServiceAccountName, saImpersonation.ServiceAccountProjectName)

	// Configure the impersonation parameters.
	// Define which service account to impersonate and what scopes the token should have
	config := impersonate.CredentialsConfig{
		TargetPrincipal: saEmail,                 // The service account to impersonate.
		Scopes:          []string{stsTokenScope}, // The desired scopes for the access token.
	}

	// Use the STS token as the source token for impersonation
	opts = append(opts, option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: stsToken, TokenType: "Bearer"})))

	// Create a token source that will provide tokens with the permissions of the impersonated service account
	ts, err := impersonate.CredentialsTokenSource(ctx, config, opts...)
	if err != nil {
		return nil, fmt.Errorf("error creating impersonated credentials: %w", err)
	}

	// Get the token.
	token, err := ts.Token()
	if err != nil {
		return nil, fmt.Errorf("error getting access token: %w", err)
	}
	return token, nil
}
