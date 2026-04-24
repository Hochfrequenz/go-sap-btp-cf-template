package btp_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/hochfrequenz/go-sap-btp-cloud-foundry-mwe/internal/btp"
)

// jwksFixture stands up an RSA keypair and an httptest server that serves
// the matching JWKS. Tests mint tokens with the key and point the validator
// at the server.
type jwksFixture struct {
	key    *rsa.PrivateKey
	server *httptest.Server
	kid    string
}

func newJWKSFixture(t *testing.T) *jwksFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	then.AssertThat(t, err, is.Nil())

	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
	kid := "test-key"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"keys":[{"kty":"RSA","kid":%q,"alg":"RS256","use":"sig","n":%q,"e":%q}]}`, kid, n, e)
	}))
	t.Cleanup(srv.Close)
	return &jwksFixture{key: key, server: srv, kid: kid}
}

func (f *jwksFixture) mint(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = f.kid
	raw, err := tok.SignedString(f.key)
	then.AssertThat(t, err, is.Nil())
	return raw
}

// newValidator stands up a server that serves JWKS at /token_keys using the
// fixture's key, and returns a validator pointed at it. clientID is the
// value tokens must carry in their "aud" claim to be accepted.
func newValidator(t *testing.T, f *jwksFixture, clientID string) (*btp.JWTValidator, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token_keys", func(w http.ResponseWriter, r *http.Request) {
		resp, err := http.Get(f.server.URL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.Copy(w, resp.Body)
	})
	wrapper := httptest.NewServer(mux)
	t.Cleanup(wrapper.Close)

	v, err := btp.NewJWTValidator(context.Background(), &btp.XSUAACredentials{URL: wrapper.URL, ClientID: clientID})
	then.AssertThat(t, err, is.Nil())
	return v, wrapper.URL
}

func Test_JWTValidator_AcceptsValidToken(t *testing.T) {
	f := newJWKSFixture(t)
	v, issuer := newValidator(t, f, "GoApp")

	raw := f.mint(t, jwt.MapClaims{
		"iss": issuer,
		"aud": "GoApp",
		"exp": time.Now().Add(time.Hour).Unix(),
		"sub": "user-1",
	})
	claims, err := v.Parse(raw)
	then.AssertThat(t, err, is.Nil())
	sub, _ := claims["sub"].(string)
	then.AssertThat(t, sub, is.EqualTo("user-1"))
}

func Test_JWTValidator_RejectsWrongAudience(t *testing.T) {
	f := newJWKSFixture(t)
	v, issuer := newValidator(t, f, "GoApp")
	raw := f.mint(t, jwt.MapClaims{
		"iss": issuer,
		"aud": "SomeoneElse",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	_, err := v.Parse(raw)
	then.AssertThat(t, err, is.Not(is.Nil()))
}

// With the iss check dropped (see NewJWTValidator doc), the actual security
// boundary on "was this token minted by our XSUAA tenant?" is signature
// verification against the JWKS URL pinned at construction time. This test
// mints a token signed with a different key (and a kid the JWKS does not
// advertise) to confirm the validator rejects it.
func Test_JWTValidator_RejectsTokenSignedByUnknownKey(t *testing.T) {
	f := newJWKSFixture(t)
	v, _ := newValidator(t, f, "GoApp")

	other, err := rsa.GenerateKey(rand.Reader, 2048)
	then.AssertThat(t, err, is.Nil())

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"aud": "GoApp",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "key-not-in-jwks"
	raw, err := tok.SignedString(other)
	then.AssertThat(t, err, is.Nil())

	_, err = v.Parse(raw)
	then.AssertThat(t, err, is.Not(is.Nil()))
}

func Test_JWTValidator_RejectsExpired(t *testing.T) {
	f := newJWKSFixture(t)
	v, issuer := newValidator(t, f, "GoApp")
	raw := f.mint(t, jwt.MapClaims{
		"iss": issuer,
		"aud": "GoApp",
		// Past the TokenRefreshLeeway window.
		"exp": time.Now().Add(-2 * time.Minute).Unix(),
	})
	_, err := v.Parse(raw)
	then.AssertThat(t, err, is.Not(is.Nil()))
	then.AssertThat(t, strings.Contains(err.Error(), "expired") || strings.Contains(err.Error(), "exp"), is.True())
}

func Test_JWTValidator_RejectsHS256(t *testing.T) {
	f := newJWKSFixture(t)
	v, _ := newValidator(t, f, "GoApp")
	// Sign with HMAC — the parser must refuse the algorithm before ever
	// reaching key lookup (classic alg-confusion defence).
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"aud": "GoApp",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	raw, err := tok.SignedString([]byte("secret"))
	then.AssertThat(t, err, is.Nil())
	_, err = v.Parse(raw)
	then.AssertThat(t, err, is.Not(is.Nil()))
}

func Test_JWTValidator_RequiresURLAndClientID(t *testing.T) {
	// Missing URL.
	_, err := btp.NewJWTValidator(context.Background(), &btp.XSUAACredentials{ClientID: "c"})
	then.AssertThat(t, err, is.Not(is.Nil()))

	// Missing ClientID.
	_, err = btp.NewJWTValidator(context.Background(), &btp.XSUAACredentials{URL: "https://u"})
	then.AssertThat(t, err, is.Not(is.Nil()))
}

func Test_JWTValidator_RequiresNonNil(t *testing.T) {
	_, err := btp.NewJWTValidator(context.Background(), nil)
	then.AssertThat(t, err, is.Not(is.Nil()))
}

func Test_Middleware_Rejects_MissingBearer(t *testing.T) {
	f := newJWKSFixture(t)
	v, _ := newValidator(t, f, "GoApp")

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(v.Middleware())
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	then.AssertThat(t, w.Code, is.EqualTo(http.StatusUnauthorized))
}

func Test_Middleware_Rejects_Malformed(t *testing.T) {
	f := newJWKSFixture(t)
	v, _ := newValidator(t, f, "GoApp")

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(v.Middleware())
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer not-a-jwt")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	then.AssertThat(t, w.Code, is.EqualTo(http.StatusUnauthorized))

	// Envelope shape for the "invalid token" branch. The raw jwt/keyfunc
	// error must NOT appear in the response body — only a stable message
	// behind the typed code.
	var env btp.ErrorEnvelope
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Error.Code, is.EqualTo(btp.CodeUnauthorized))
	then.AssertThat(t, env.Error.Message, is.EqualTo("invalid or expired token"))
	// jwt/v5 library internals should never leak.
	then.AssertThat(t, strings.Contains(w.Body.String(), "token is malformed"),
		is.False())
}

func Test_Middleware_AcceptsAndStashesToken(t *testing.T) {
	f := newJWKSFixture(t)
	v, issuer := newValidator(t, f, "GoApp")

	raw := f.mint(t, jwt.MapClaims{
		"iss": issuer,
		"aud": "GoApp",
		"exp": time.Now().Add(time.Hour).Unix(),
		"sub": "u",
	})

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(v.Middleware())
	r.GET("/x", func(c *gin.Context) {
		tokStr, _ := c.Request.Context().Value(btp.ForwardedUserTokenKey{}).(string)
		_, ok := c.Get("jwtClaims")
		then.AssertThat(t, ok, is.True())
		then.AssertThat(t, tokStr, is.EqualTo(raw))
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))
}
