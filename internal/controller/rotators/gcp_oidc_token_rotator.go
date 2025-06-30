// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package rotators

import (
	"context"
	"fmt"
	"log"
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
	gcpAccessTokenKey      = "gcpAccessToken"
	grantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange" // nolint:gosec
	gcpIAMScope            = "https://www.googleapis.com/auth/iam"             // nolint:gosec
	tokenTypeAccessToken   = "urn:ietf:params:oauth:token-type:access_token"   // nolint:gosec
	tokenTypeJWT           = "urn:ietf:params:oauth:token-type:jwt"            // nolint:gosec
	stsTokenScope          = "https://www.googleapis.com/auth/cloud-platform"  // nolint:gosec
)

// gcpOIDCTokenRotator implements Rotator interface for GCP access token exchange.
type gcpOIDCTokenRotator struct {
	client client.Client
	kube   kubernetes.Interface
	logger logr.Logger
	// BackendSecurityPolicy
	gcpCredentials aigv1a1.BackendSecurityPolicyGCPCredentials
	// backendSecurityPolicyName provides name of backend security policy.
	backendSecurityPolicyName string
	// backendSecurityPolicyNamespace provides namespace of backend security policy.
	backendSecurityPolicyNamespace string
	preRotationWindow              time.Duration
	oidcProvider                   tokenprovider.TokenProvider
}

// NewGCPOIDCTokenRotator creates a new gcpOIDCTokenRotator with the given parameters.
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

	oidcConfig := bsp.Spec.GCPCredentials.WorkLoadIdentityFederationConfig.WorkloadIdentityProvider.OIDCProvider.OIDC
	oidcProvider, err := tokenprovider.NewOidcTokenProvider(ctx, client, &oidcConfig)
	if err != nil {
		logger.Error(err, "failed to construct oidc provider")
		return nil, err
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
func (r *gcpOIDCTokenRotator) IsExpired(preRotationExpirationTime time.Time) bool {
	return IsBufferedTimeExpired(0, preRotationExpirationTime)
}

// GetPreRotationTime implements Rotator.GetPreRotationTime method to retrieve the pre-rotation time for GCP token.
func (r *gcpOIDCTokenRotator) GetPreRotationTime(ctx context.Context) (time.Time, error) {
	secret, err := LookupSecret(ctx, r.client, r.backendSecurityPolicyNamespace, GetBSPSecretName(r.backendSecurityPolicyName))
	if err != nil {
		if apierrors.IsNotFound(err) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	expirationTime, err := GetExpirationSecretAnnotation(secret)
	if err != nil {
		return time.Time{}, err
	}
	preRotationTime := expirationTime.Add(-r.preRotationWindow)
	return preRotationTime, nil
}

// Rotate implements Rotator.Rotate method to rotate GCP access token and updates the Kubernetes secret.
func (r *gcpOIDCTokenRotator) Rotate(ctx context.Context) (time.Time, error) {
	secretName := GetBSPSecretName(r.backendSecurityPolicyName)

	r.logger.Info("start rotating gcp access token", "namespace", r.backendSecurityPolicyNamespace, "name", r.backendSecurityPolicyName)

	// 1. Get OIDCProvider Token
	oidcTokenExpiry, err := r.oidcProvider.GetToken(ctx)
	if err != nil {
		r.logger.Error(err, "failed to get token from oidc provider", "oidcIssuer", r.gcpCredentials.WorkLoadIdentityFederationConfig.WorkloadIdentityProvider.Name)
		return time.Time{}, err
	}

	// 2. Exchange the JWT for an STS token.
	stsToken, err := r.exchangeJWTForSTSToken(ctx, oidcTokenExpiry.Token) // Replace
	if err != nil {
		log.Fatalf("Error exchanging JWT for STS token: %v", err)
	}

	// 3. Exchange the STS token for a GCP service account access token.
	gcpAccessToken, err := r.impersonateServiceAccount(context.Background(), stsToken)
	if err != nil {
		log.Fatalf("Error exchanging STS token for GCP access token: %v", err)
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
func (r *gcpOIDCTokenRotator) exchangeJWTForSTSToken(ctx context.Context, jwtToken string, opts ...option.ClientOption) (string, error) {
	// Create an STS client.
	opts = append(opts, option.WithoutAuthentication())
	stsService, err := sts.NewService(ctx, opts...)
	if err != nil {
		return "", fmt.Errorf("error creating STS service: %w", err)
	}
	// Construct the STS request.

	wifConfig := r.gcpCredentials.WorkLoadIdentityFederationConfig
	stsAudience := fmt.Sprintf("//iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s/providers/%s", wifConfig.ProjectID, wifConfig.WorkloadIdentityPoolName, wifConfig.WorkloadIdentityProvider.Name)

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
func (r *gcpOIDCTokenRotator) impersonateServiceAccount(ctx context.Context, stsToken string, opts ...option.ClientOption) (*oauth2.Token, error) {
	saImpersonation := r.gcpCredentials.WorkLoadIdentityFederationConfig.ServiceAccountImpersonation
	saEmail := fmt.Sprintf("%s@%s.iam.gserviceaccount.com", saImpersonation.ServiceAccountName, saImpersonation.ServiceAccountProjectName)

	// Configure the impersonation parameters.
	config := impersonate.CredentialsConfig{
		TargetPrincipal: saEmail,                 // The service account to impersonate.
		Scopes:          []string{stsTokenScope}, // The desired scopes for the access token.
	}

	// Use the ImpersonateCredentials function.
	opts = append(opts, option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: stsToken, TokenType: "Bearer"})))
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
