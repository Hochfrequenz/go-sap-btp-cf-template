package btp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"

	"github.com/hochfrequenz/go-sap-btp-cf-template/internal/btp"
)

func newReq(t *testing.T) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "http://example/", nil)
	return r
}

func Test_BasicAuthenticator_Applies(t *testing.T) {
	reg := btp.DefaultAuthenticators()
	r := newReq(t)
	err := reg.Apply(context.Background(), r, &btp.Destination{
		Name:           "D",
		Authentication: "BasicAuthentication",
		User:           "alice",
		Password:       "secret",
	})
	then.AssertThat(t, err, is.Nil())
	u, p, ok := r.BasicAuth()
	then.AssertThat(t, ok, is.True())
	then.AssertThat(t, u, is.EqualTo("alice"))
	then.AssertThat(t, p, is.EqualTo("secret"))
}

func Test_BasicAuthenticator_RejectsColonInUser(t *testing.T) {
	reg := btp.DefaultAuthenticators()
	err := reg.Apply(context.Background(), newReq(t), &btp.Destination{
		Name:           "D",
		Authentication: "BasicAuthentication",
		User:           "a:b",
		Password:       "p",
	})
	then.AssertThat(t, err, is.Not(is.Nil()))
}

func Test_NoAuthenticator_LeavesRequestAlone(t *testing.T) {
	reg := btp.DefaultAuthenticators()
	r := newReq(t)
	err := reg.Apply(context.Background(), r, &btp.Destination{Name: "D", Authentication: "NoAuthentication"})
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, r.Header.Get("Authorization"), is.EqualTo(""))
}

func Test_EmptyAuthTypeIsAccepted(t *testing.T) {
	reg := btp.DefaultAuthenticators()
	err := reg.Apply(context.Background(), newReq(t), &btp.Destination{Name: "D"})
	then.AssertThat(t, err, is.Nil())
}

func Test_UnknownAuthTypeIsRejected(t *testing.T) {
	reg := btp.DefaultAuthenticators()
	err := reg.Apply(context.Background(), newReq(t), &btp.Destination{Name: "D", Authentication: "SomethingNew"})
	then.AssertThat(t, err, is.Not(is.Nil()))
	then.AssertThat(t, strings.Contains(err.Error(), "SomethingNew"), is.True())
}

type stubAuth struct{ called *bool }

func (s stubAuth) AuthType() btp.AuthType { return "Custom" }
func (s stubAuth) Apply(_ context.Context, r *http.Request, _ *btp.Destination) error {
	*s.called = true
	r.Header.Set("X-Custom", "1")
	return nil
}

func Test_Registry_AcceptsCustomAuthenticator(t *testing.T) {
	reg := btp.DefaultAuthenticators()
	called := false
	reg.Register(stubAuth{called: &called})

	r := newReq(t)
	err := reg.Apply(context.Background(), r, &btp.Destination{Name: "D", Authentication: "Custom"})
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, called, is.True())
	then.AssertThat(t, r.Header.Get("X-Custom"), is.EqualTo("1"))
}
