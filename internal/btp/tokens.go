package btp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// TokenRefreshLeeway is subtracted from the server-reported TTL when deciding
// whether a cached token is still usable. Without leeway, a token fetched
// milliseconds before expiry will be handed out, travel across the network,
// and be rejected on arrival. The PHP reference has had multiple bugs of
// this shape — the article the MWE is built from calls them out explicitly.
const TokenRefreshLeeway = 30 * time.Second

// TokenFetcher obtains and caches XSUAA client-credentials tokens.
// One instance is safe for concurrent use and serves multiple bindings
// (keyed on tokenURL + clientID). Concurrent misses collapse into a single
// upstream exchange via singleflight — otherwise a burst of incoming
// requests during a token TTL gap would hammer XSUAA and risk rate-limits.
type TokenFetcher struct {
	httpClient *http.Client
	group      singleflight.Group

	mu    sync.Mutex
	cache map[string]cachedToken
}

// NewTokenFetcher returns a fetcher. If httpClient is nil a sensible default
// with a 10s timeout is used.
func NewTokenFetcher(httpClient *http.Client) *TokenFetcher {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &TokenFetcher{
		httpClient: httpClient,
		cache:      map[string]cachedToken{},
	}
}

// Fetch returns a cached token if one is still valid (minus leeway), else
// performs a fresh client-credentials exchange. tokenBaseURL is the `url`
// field of the service binding; /oauth/token is appended.
func (f *TokenFetcher) Fetch(ctx context.Context, tokenBaseURL, clientID, clientSecret string) (string, error) {
	if tokenBaseURL == "" || clientID == "" || clientSecret == "" {
		return "", errors.New("token fetch: tokenBaseURL, clientID and clientSecret are required")
	}
	key := tokenBaseURL + "|" + clientID

	f.mu.Lock()
	t, ok := f.cache[key]
	f.mu.Unlock()
	if ok && time.Now().Add(TokenRefreshLeeway).Before(t.expiresAt) {
		return t.token, nil
	}

	// singleflight.Do dedupes concurrent fetches for the same key — the
	// first caller triggers the exchange, peers wait and receive its result.
	v, err, _ := f.group.Do(key, func() (any, error) {
		// A second cache read inside the critical section catches the case
		// where a peer finished the exchange while we were waiting.
		f.mu.Lock()
		t, ok := f.cache[key]
		f.mu.Unlock()
		if ok && time.Now().Add(TokenRefreshLeeway).Before(t.expiresAt) {
			return t.token, nil
		}
		tok, ttl, err := f.exchange(ctx, tokenBaseURL, clientID, clientSecret)
		if err != nil {
			return "", err
		}
		f.mu.Lock()
		f.cache[key] = cachedToken{token: tok, expiresAt: time.Now().Add(ttl)}
		f.mu.Unlock()
		return tok, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// Invalidate drops the cached token for (tokenBaseURL, clientID). Call this
// when a downstream call rejected the token (401/403) so the next Fetch
// obtains a fresh one rather than re-handing the bad one back.
func (f *TokenFetcher) Invalidate(tokenBaseURL, clientID string) {
	f.mu.Lock()
	delete(f.cache, tokenBaseURL+"|"+clientID)
	f.mu.Unlock()
}

func (f *TokenFetcher) exchange(ctx context.Context, tokenBaseURL, clientID, clientSecret string) (string, time.Duration, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")

	endpoint := trimSlash(tokenBaseURL) + "/oauth/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(clientID, clientSecret)

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMgmtResponseBytes))
	if err != nil {
		return "", 0, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return "", 0, fmt.Errorf("token endpoint %s returned %d", endpoint, resp.StatusCode)
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", 0, fmt.Errorf("decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", 0, errors.New("token endpoint returned empty access_token")
	}
	return tr.AccessToken, time.Duration(tr.ExpiresIn) * time.Second, nil
}
