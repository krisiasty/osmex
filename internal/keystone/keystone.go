// Package keystone polls the Identity API for the expiry of the exporter's own application credential.
package keystone

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/krisiasty/osmex/internal/expo"
	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/poller"
)

// Spec identifies the Identity API.
var Spec = osclient.ServiceSpec{Types: []string{"identity"}, Suffix: "/v3"}

// CredentialPoller is the name of the poller NewCredential returns.
const CredentialPoller = "keystone-credential"

// CredentialInterval is how often the credential is read. Application credentials are immutable, so this only bounds
// how long a failed read goes unretried.
const CredentialInterval = time.Hour

// NewCredential returns the keystone-credential poller, which reads application credential credentialID of the
// token's user and reports when it expires. A credential without an expiry produces no series.
func NewCredential(svc *osclient.ServiceClient, credentialID string, interval time.Duration, opts ...poller.Option) *poller.Poller {
	return poller.New(CredentialPoller, func() time.Duration { return interval }, func(ctx context.Context) ([]byte, error) {
		tok, err := svc.Token(ctx)
		if err != nil {
			return nil, err
		}
		if tok.UserID == "" {
			return nil, decodeError(ctx, "token has no user id")
		}
		var out struct {
			Credential *struct {
				ExpiresAt *string `json:"expires_at"`
			} `json:"application_credential"`
		}
		path := "/users/" + url.PathEscape(tok.UserID) + "/application_credentials/" + url.PathEscape(credentialID)
		if err := svc.Get(ctx, path, nil, &out); err != nil {
			return nil, err
		}
		if out.Credential == nil {
			return nil, decodeError(ctx, "response has no application_credential")
		}
		// Only a null or missing expires_at means the credential never expires. An empty string is malformed: taking it
		// for "never" would erase a known expiry.
		var page expo.Page
		if out.Credential.ExpiresAt != nil {
			t, err := osclient.ParseTime(*out.Credential.ExpiresAt)
			switch {
			case err != nil:
				err = &osclient.DecodeError{Err: err}
				osclient.RecordErrorCode(ctx, osclient.ErrorCode(err))
				return nil, err
			case t.IsZero():
				return nil, decodeError(ctx, "application credential has an empty expires_at")
			}
			page.Gauge("openstack_exporter_credential_expiration_timestamp_seconds",
				"Time after which the application credential can no longer authenticate.").Add(float64(t.UnixNano())/1e9)
		}
		return page.Bytes(), nil
	}, opts...)
}

func decodeError(ctx context.Context, msg string) error {
	err := &osclient.DecodeError{Err: errors.New(msg)}
	osclient.RecordErrorCode(ctx, osclient.ErrorCode(err))
	return err
}
