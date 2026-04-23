package btp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// JWTValidator validates tokens minted by this app's XSUAA tenant. Construct
// once at startup; safe for concurrent use. The underlying keyfunc keeps the
// JWKS fresh on a background refresh loop.
type JWTValidator struct {
	xsuaa   *XSUAACredentials
	keyfunc jwt.Keyfunc
	parser  *jwt.Parser
}

// NewJWTValidator fetches the JWKS at xsuaa.JWKSURL() and returns a ready
// validator. It verifies: RS256 signature (keys pinned to xsuaa.JWKSURL()),
// audience = xsuaa.ClientID, standard exp/nbf/iat with TokenRefreshLeeway.
//
// The issuer claim is intentionally not checked. XSUAA emits an internal
// "http://<zone>.localhost:8080/uaa/oauth/token" iss that cannot be
// derived from VCAP_SERVICES without hardcoding a SAP implementation
// detail. Security is preserved by deriving the JWKS URL from this app's
// own XSUAA binding (xsuaa.URL + "/token_keys"), so the keyset only ever
// contains our tenant's signing keys; a token minted by a different
// tenant fails signature verification. Callers must keep that
// invariant — do not let the JWKS URL come from anywhere but the bound
// XSUAACredentials, or the iss-drop argument no longer holds.
func NewJWTValidator(ctx context.Context, xsuaa *XSUAACredentials) (*JWTValidator, error) {
	if xsuaa == nil {
		return nil, errors.New("xsuaa credentials required")
	}
	if xsuaa.URL == "" || xsuaa.ClientID == "" {
		return nil, errors.New("xsuaa credentials missing url or clientid")
	}

	kf, err := keyfunc.NewDefaultCtx(ctx, []string{xsuaa.JWKSURL()})
	if err != nil {
		return nil, fmt.Errorf("fetch jwks from %s: %w", xsuaa.JWKSURL(), err)
	}

	parser := jwt.NewParser(
		// RS256 is the only algorithm XSUAA signs with. Enforcing it
		// explicitly blocks the "alg: none" and "HS256 with the public key
		// as secret" classic confusion attacks.
		jwt.WithValidMethods([]string{"RS256"}),
		// Real XSUAA tokens carry aud entries like "sb-<xsappname>!t<tenant>"
		// (the clientid form), not the bare xsappname. Comparing against
		// ClientID matches what XSUAA actually emits.
		jwt.WithAudience(xsuaa.ClientID),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(TokenRefreshLeeway),
	)

	return &JWTValidator{xsuaa: xsuaa, keyfunc: kf.Keyfunc, parser: parser}, nil
}

func (v *JWTValidator) Parse(raw string) (jwt.MapClaims, error) {
	claims := jwt.MapClaims{}
	if _, err := v.parser.ParseWithClaims(raw, claims, v.keyfunc); err != nil {
		return nil, err
	}
	return claims, nil
}

// Middleware enforces a valid JWT on Authorization: Bearer. The raw token is
// stashed in the request context under ForwardedUserTokenKey{} so downstream
// authenticators (PrincipalPropagation) can reuse it; parsed claims land in
// the Gin context as "jwtClaims".
func (v *JWTValidator) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.GetHeader("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		raw := strings.TrimPrefix(h, "Bearer ")
		claims, err := v.Parse(raw)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token: " + err.Error()})
			return
		}
		c.Set("jwtClaims", claims)
		ctx := context.WithValue(c.Request.Context(), ForwardedUserTokenKey{}, raw)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
