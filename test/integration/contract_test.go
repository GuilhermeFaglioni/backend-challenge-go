//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/api"
)

// contractClient is an HTTP client that checks every exchange against the OpenAPI
// document: the response always, and the request too unless the test sends a
// deliberately invalid one. A handler that answers with a status, a header or a body
// the document does not describe makes the exchange a violation.
type contractClient struct {
	http *http.Client
	t    *testing.T

	mu         sync.Mutex
	violations []string
}

type contractTransport struct {
	base          http.RoundTripper
	router        routers.Router
	c             *contractClient
	validateInput bool
}

func newContractClient(t *testing.T, baseURL string) *contractClient {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData(api.OpenAPI)
	require.NoError(t, err)
	require.NoError(t, doc.Validate(t.Context()))
	doc.Servers = openapi3.Servers{{URL: baseURL}}
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	c := &contractClient{t: t}
	c.http = &http.Client{Transport: &contractTransport{base: http.DefaultTransport.(*http.Transport).Clone(), router: router, c: c, validateInput: true}}
	return c
}

// lenient returns a client that does not hold the requests to the document, for the
// tests that send invalid ones on purpose. The responses are still checked.
func (c *contractClient) lenient() *contractClient {
	t := c.http.Transport.(*contractTransport)
	return &contractClient{t: c.t, http: &http.Client{Transport: &contractTransport{base: t.base, router: t.router, c: c, validateInput: false}}}
}

func (c *contractClient) record(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.violations = append(c.violations, fmt.Sprintf(format, args...))
}

func (c *contractClient) requireNoViolations() {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	require.Empty(c.t, c.violations, "the API deviated from openapi.yaml")
}

func (tr *contractTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	route, pathParams, err := tr.router.FindRoute(req)
	if err != nil {
		tr.c.record("%s %s is not in the document: %v", req.Method, req.URL.Path, err)
		return tr.base.RoundTrip(req)
	}
	input := &openapi3filter.RequestValidationInput{
		Request: req, PathParams: pathParams, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	}
	if tr.validateInput {
		if err := openapi3filter.ValidateRequest(req.Context(), input); err != nil {
			tr.c.record("request %s %s: %v", req.Method, req.URL.Path, err)
		}
	}

	resp, err := tr.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))

	responseInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: input, Status: resp.StatusCode, Header: resp.Header,
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}
	responseInput.SetBodyBytes(body)
	if err := openapi3filter.ValidateResponse(req.Context(), responseInput); err != nil {
		tr.c.record("response to %s %s (%d): %v", req.Method, req.URL.Path, resp.StatusCode, err)
	}
	return resp, nil
}

// call sends a request and returns the status and the body.
func (c *contractClient) call(ctx context.Context, method, url, token, body string, headers ...string) (int, http.Header, string) {
	c.t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	require.NoError(c.t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.http.Do(req)
	require.NoError(c.t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	return resp.StatusCode, resp.Header, string(data)
}

// The validator must be able to fail, or every contract test below proves nothing.
func TestTheContractCheckCatchesAHandlerThatDeviates(t *testing.T) {
	deviations := map[string]http.HandlerFunc{
		"a status the document does not list": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTeapot)
			_, _ = io.WriteString(w, `{}`)
		},
		"a field missing from the body": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"7c9e6679-7425-40de-944b-e07fc1f90ae7","version":1}`)
		},
		"money as a number": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"7c9e6679-7425-40de-944b-e07fc1f90ae7","playerId":"7c9e6679-7425-40de-944b-e07fc1f90ae7","balance":{"amount":10.5,"currency":"BRL"},"version":1}`)
		},
		"an amount with one decimal": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"7c9e6679-7425-40de-944b-e07fc1f90ae7","playerId":"7c9e6679-7425-40de-944b-e07fc1f90ae7","balance":{"amount":"10.5","currency":"BRL"},"version":1}`)
		},
		"an error code the document does not list": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"WHO_KNOWS","message":"x"}}`)
		},
	}
	for name, handler := range deviations {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			defer server.Close()
			c := newContractClient(t, server.URL)
			c.call(t.Context(), http.MethodGet, server.URL+"/wallets/7c9e6679-7425-40de-944b-e07fc1f90ae7", "token", "")
			require.NotEmpty(t, c.violations, "the deviation went unnoticed")
		})
	}
}
