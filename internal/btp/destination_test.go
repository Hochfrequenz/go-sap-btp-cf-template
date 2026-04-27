package btp_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"

	"github.com/hochfrequenz/go-sap-btp-cf-template/internal/btp"
)

func Test_LookupDestination_EnvelopeResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		then.AssertThat(t, r.Header.Get("Authorization"), is.EqualTo("Bearer t"))
		then.AssertThat(t, r.URL.Path, is.EqualTo("/destination-configuration/v1/destinations/MyDest"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"destinationConfiguration": {
				"Name": "MyDest",
				"Type": "HTTP",
				"URL": "http://sap.internal:8000",
				"Authentication": "BasicAuthentication",
				"ProxyType": "OnPremise",
				"User": "u",
				"Password": "p",
				"CloudConnectorLocationId": "loc-1"
			},
			"authTokens": []
		}`))
	}))
	defer srv.Close()

	cred := &btp.DestCredentials{URI: srv.URL, ClientID: "x", ClientSecret: "y", URL: "https://xsuaa.example"}
	d, err := btp.LookupDestination(context.Background(), srv.Client(), cred, "t", "MyDest")
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, d.URL, is.EqualTo("http://sap.internal:8000"))
	then.AssertThat(t, d.User, is.EqualTo("u"))
	then.AssertThat(t, d.IsOnPremise(), is.True())
	then.AssertThat(t, d.CloudConnectorLocationID, is.EqualTo("loc-1"))
}

func Test_LookupDestination_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cred := &btp.DestCredentials{URI: srv.URL}
	_, err := btp.LookupDestination(context.Background(), srv.Client(), cred, "t", "Missing")
	then.AssertThat(t, err, is.Not(is.Nil()))
	then.AssertThat(t, strings.Contains(err.Error(), "not found"), is.True())
}

func Test_LookupDestination_RequiresName(t *testing.T) {
	cred := &btp.DestCredentials{URI: "http://example"}
	_, err := btp.LookupDestination(context.Background(), nil, cred, "t", "")
	then.AssertThat(t, err, is.Not(is.Nil()))
}

func Test_LookupDestination_RequiresBinding(t *testing.T) {
	_, err := btp.LookupDestination(context.Background(), nil, nil, "t", "X")
	then.AssertThat(t, err, is.Not(is.Nil()))
}

func Test_LookupDestination_AcceptsDirectResponse(t *testing.T) {
	// Older destination-service surfaces return the Destination record at
	// the top level, without the destinationConfiguration envelope.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Name":"D","Type":"HTTP","URL":"http://s","Authentication":"NoAuthentication","ProxyType":"Internet"}`))
	}))
	defer srv.Close()

	cred := &btp.DestCredentials{URI: srv.URL}
	d, err := btp.LookupDestination(context.Background(), srv.Client(), cred, "t", "D")
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, d.URL, is.EqualTo("http://s"))
	then.AssertThat(t, d.IsOnPremise(), is.False())
}

func Test_LookupDestination_PropagatesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cred := &btp.DestCredentials{URI: srv.URL}
	_, err := btp.LookupDestination(context.Background(), srv.Client(), cred, "t", "D")
	then.AssertThat(t, err, is.Not(is.Nil()))
	then.AssertThat(t, strings.Contains(err.Error(), "500"), is.True())
}

func Test_Destination_StringScrubsPassword(t *testing.T) {
	d := &btp.Destination{
		Name: "MyDest", Type: "HTTP", URL: "http://sap.internal:8000",
		Authentication: btp.AuthBasic, ProxyType: btp.ProxyOnPremise,
		User: "u", Password: "super-secret", CloudConnectorLocationID: "loc-1",
	}

	// Cover %v, %+v, and %#v — all three are formatter routes a caller
	// might reach for in a log line. Format() funnels every verb through
	// String() so they all mask.
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		s := fmt.Sprintf(verb, d)
		then.AssertThat(t, strings.Contains(s, "super-secret"), is.False())
		then.AssertThat(t, strings.Contains(s, "***"), is.True())
	}

	// slog routes the value through fmt.Sprintf("%+v", v) for non-LogValuer
	// types; the same masking must hold there.
	then.AssertThat(t,
		strings.Contains(fmt.Sprintf("dest=%+v", d), "super-secret"), is.False())
}

func Test_Destination_NilStringIsSafe(t *testing.T) {
	var d *btp.Destination
	then.AssertThat(t, strings.Contains(fmt.Sprintf("%v", d), "nil"), is.True())
}
