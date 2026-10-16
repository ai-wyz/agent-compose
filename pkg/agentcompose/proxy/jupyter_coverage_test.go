package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/chaitin/agent-compose/pkg/fxgo/echofn"
	domain "github.com/chaitin/agent-compose/pkg/model"
	"github.com/chaitin/agent-compose/pkg/sandboxes"
)

// A sandbox that does not exist is a missing resource, not a daemon fault, and
// the route that answers it is exempt from daemon authentication. Neither branch
// may disclose where daemon-side state lives, so the body stays path-free and the
// failure detail goes to the log instead. The one message clients already match on
// is the "jupyter is not enabled" condition, which names the session only.
func TestJupyterProxyAnswersWithoutLeakingStoragePaths(t *testing.T) {
	cases := []struct {
		name       string
		ensureErr  error
		wantStatus int
		wantMsg    string
		wantErrTyp string
		// The detail the body must not carry, which the log must keep instead.
		wantLogged string
	}{
		{
			name:       "missing sandbox is not found",
			ensureErr:  domain.ResourceError(domain.ErrNotFound, "sandbox", "deadbeef", "", nil),
			wantStatus: http.StatusNotFound,
			wantMsg:    "sandbox deadbeef not found",
			wantErrTyp: "Unknown",
		},
		{
			name: "wrapped storage not-exist without classification stays unavailable",
			ensureErr: fmt.Errorf(
				"read session metadata deadbeef: open /data/sandboxes/deadbeef/metadata.json: %w", fs.ErrNotExist),
			wantStatus: http.StatusBadGateway,
			wantMsg:    "jupyter is unavailable for sandbox deadbeef",
			wantErrTyp: "Internal",
			wantLogged: "/data/sandboxes/deadbeef/metadata.json",
		},
		{
			name: "jupyter disabled keeps its message",
			ensureErr: domain.ClassifyError(
				sandboxes.ErrJupyterNotEnabled, "jupyter is not enabled for session deadbeef", nil),
			wantStatus: http.StatusBadGateway,
			wantMsg:    "jupyter is not enabled for session deadbeef",
			wantErrTyp: "Internal",
		},
		{
			// A provisioning failure means the upstream is unavailable, not that
			// the sandbox is absent, so it must not be folded into the 404 branch.
			name:       "provisioning failure stays bad gateway",
			ensureErr:  fmt.Errorf("ensure workspace: read /data/sandboxes/deadbeef/workspace: %w", fs.ErrNotExist),
			wantStatus: http.StatusBadGateway,
			wantMsg:    "jupyter is unavailable for sandbox deadbeef",
			wantErrTyp: "Internal",
			wantLogged: "/data/sandboxes/deadbeef/workspace",
		},
	}
	for _, tc := range cases {
		for _, path := range []string{"/jupyter/deadbeef", "/jupyter/deadbeef/api/status"} {
			t.Run(tc.name+" "+path, func(t *testing.T) {
				var logs bytes.Buffer
				e := echo.New()
				e.HTTPErrorHandler = echofn.EchoHTTPErrorHandler
				RegisterJupyterRoutes(e, JupyterOptions{
					BasePath: "/jupyter",
					Store:    fakeJupyterStore{err: tc.ensureErr},
					Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
					EnsureReady: func(context.Context, string) (domain.ProxyState, error) {
						return domain.ProxyState{}, tc.ensureErr
					},
				})
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				body := rec.Body.String()
				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, body)
				}
				if !strings.Contains(body, tc.wantMsg) {
					t.Fatalf("body = %s, want message %q", body, tc.wantMsg)
				}
				if !strings.Contains(body, `"err":"`+tc.wantErrTyp+`"`) {
					t.Fatalf("body = %s, want err type %q", body, tc.wantErrTyp)
				}
				for _, leaked := range []string{"/", "metadata.json", "sandboxes", "session metadata"} {
					if strings.Contains(body, leaked) {
						t.Fatalf("body leaks %q: %s", leaked, body)
					}
				}
				if tc.wantLogged == "" {
					if logs.Len() != 0 {
						t.Fatalf("classified failure logged %q, want nothing", logs.String())
					}
					return
				}
				if !strings.Contains(logs.String(), tc.wantLogged) {
					t.Fatalf("log = %q, want the withheld detail %q", logs.String(), tc.wantLogged)
				}
			})
		}
	}
}

func TestJupyterProxyRouteCoverage(t *testing.T) {
	e := echo.New()
	store := &fakeJupyterStore{state: domain.ProxyState{ProxyPath: "/agent-compose/session/session-1", Token: "token value"}}
	var ensureCalls int
	RegisterJupyterRoutes(e, JupyterOptions{
		BasePath: "/jupyter/",
		Store:    store,
		EnsureReady: func(context.Context, string) (domain.ProxyState, error) {
			ensureCalls++
			return store.state, nil
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/jupyter/session-1", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect || !strings.Contains(rec.Header().Get("Location"), "token=token+value") || ensureCalls != 1 {
		t.Fatalf("redirect status=%d location=%q ensure=%d", rec.Code, rec.Header().Get("Location"), ensureCalls)
	}

	store.err = errors.New("proxy state missing")
	req = httptest.NewRequest(http.MethodPost, "/jupyter/session-1/api/kernels", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("store error status=%d body=%s", rec.Code, rec.Body.String())
	}

	failing := echo.New()
	RegisterJupyterRoutes(failing, JupyterOptions{
		BasePath: "/jupyter",
		Store:    fakeJupyterStore{err: errors.New("missing")},
		EnsureReady: func(context.Context, string) (domain.ProxyState, error) {
			return domain.ProxyState{}, errors.New("ensure failed")
		},
	})
	req = httptest.NewRequest(http.MethodGet, "/jupyter/session-1", nil)
	rec = httptest.NewRecorder()
	failing.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("ensure redirect failure status=%d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/jupyter/session-1/api/kernels", nil)
	rec = httptest.NewRecorder()
	failing.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("ensure proxy failure status=%d body=%s", rec.Code, rec.Body.String())
	}

	if JupyterTargetReachable(domain.ProxyState{}, time.Millisecond) {
		t.Fatalf("empty proxy state reported reachable")
	}
}

func TestJupyterProxyPreservesPublicHostForSameOriginChecks(t *testing.T) {
	var gotHost, gotOrigin string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotOrigin = r.Header.Get("Origin")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(backendURL.Port())
	if err != nil {
		t.Fatal(err)
	}

	e := echo.New()
	state := domain.ProxyState{Enabled: true, GuestHost: "127.0.0.1", GuestPort: port, HostPort: port, ProxyPath: "/jupyter/session-1"}
	RegisterJupyterRoutes(e, JupyterOptions{
		BasePath: "/jupyter",
		Store:    fakeJupyterStore{state: state},
		EnsureReady: func(context.Context, string) (domain.ProxyState, error) {
			return state, nil
		},
	})
	request := httptest.NewRequest(http.MethodPost, "/jupyter/session-1/api/kernels", nil)
	request.Host = "proxy.example.test"
	request.Header.Set("Origin", "http://proxy.example.test")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("proxy status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if gotHost != request.Host || gotOrigin != request.Header.Get("Origin") {
		t.Fatalf("backend host/origin = %q/%q, want %q/%q", gotHost, gotOrigin, request.Host, request.Header.Get("Origin"))
	}
}

func TestNewJupyterProxyTransportDisablesProxyFromEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")

	transport := newJupyterProxyTransport()
	if transport.Proxy != nil {
		t.Fatalf("jupyter proxy transport should not use proxy environment")
	}
}

type fakeJupyterStore struct {
	state domain.ProxyState
	err   error
}

func (s fakeJupyterStore) GetProxyState(string) (domain.ProxyState, error) {
	if s.err != nil {
		return domain.ProxyState{}, s.err
	}
	return s.state, nil
}
