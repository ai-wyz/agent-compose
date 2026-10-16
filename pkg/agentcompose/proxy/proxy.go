package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	driverpkg "github.com/chaitin/agent-compose/pkg/driver"
	"github.com/chaitin/agent-compose/pkg/execution"
	domain "github.com/chaitin/agent-compose/pkg/model"
	"github.com/chaitin/agent-compose/pkg/sandboxes"
)

type Store interface {
	GetProxyState(id string) (domain.ProxyState, error)
}

type EnsureReadyFunc func(ctx context.Context, sessionID string) (domain.ProxyState, error)

type JupyterOptions struct {
	BasePath    string
	Store       Store
	EnsureReady EnsureReadyFunc
}

func RegisterJupyterRoutes(app *echo.Echo, opts JupyterOptions) {
	base := strings.TrimRight(opts.BasePath, "/")
	transport := newJupyterProxyTransport()
	app.GET(base+"/:sessionID", func(c echo.Context) error {
		sessionID := c.Param("sessionID")
		proxyState, err := opts.EnsureReady(c.Request().Context(), sessionID)
		if err != nil {
			return jupyterEnsureReadyHTTPError(sessionID, err)
		}
		location := strings.TrimRight(proxyState.ProxyPath, "/")
		if proxyState.Token != "" {
			location += "?token=" + url.QueryEscape(proxyState.Token)
		}
		return c.Redirect(http.StatusTemporaryRedirect, location)
	})
	app.Any(base+"/:sessionID/*", func(c echo.Context) error {
		sessionID := c.Param("sessionID")
		if !JupyterTargetReachable(func() domain.ProxyState {
			proxyState, err := opts.Store.GetProxyState(sessionID)
			if err != nil {
				return domain.ProxyState{}
			}
			return proxyState
		}(), 250*time.Millisecond) {
			if _, err := opts.EnsureReady(c.Request().Context(), sessionID); err != nil {
				return jupyterEnsureReadyHTTPError(sessionID, err)
			}
		}
		proxyState, err := opts.Store.GetProxyState(sessionID)
		if err != nil {
			// The store's error embeds the sandbox's absolute proxy-state path,
			// which must not reach this unauthenticated route. Keep the not-found
			// status the route already reported and answer with the id alone.
			return echo.NewHTTPError(http.StatusNotFound, missingSandboxMessage(sessionID))
		}
		target, err := url.Parse("http://" + driverpkg.JupyterConnectAddress(execution.ToDriverProxyState(proxyState)))
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		proxy := &httputil.ReverseProxy{
			Transport: transport,
			Rewrite: func(req *httputil.ProxyRequest) {
				req.SetURL(target)
				req.SetXForwarded()
				// Preserve the public proxy host so Jupyter's same-origin and
				// XSRF checks see the same host as the browser Origin header.
				req.Out.Host = req.In.Host
				req.Out.URL.Path = req.In.URL.Path
				req.Out.URL.RawPath = req.Out.URL.Path
				req.Out.URL.RawQuery = req.In.URL.RawQuery
			},
			ErrorHandler: func(rw http.ResponseWriter, req *http.Request, proxyErr error) {
				rw.WriteHeader(http.StatusBadGateway)
				_, _ = rw.Write([]byte(proxyErr.Error()))
			},
		}
		proxy.ServeHTTP(c.Response(), c.Request())
		return nil
	})
}

// jupyterEnsureReadyHTTPError maps a failed proxy readiness check to the HTTP
// error this route returns. A sandbox that does not exist is a missing resource
// (404); every other failure — Jupyter disabled, an unreachable guest, a runtime
// that will not start — keeps its 502 because the upstream is unavailable rather
// than the resource missing. Only the domain classification decides this: a raw
// filesystem error from sandbox provisioning also means "unavailable", and the
// lifecycle layer reports a genuinely missing sandbox as a domain not-found. The
// 502 branch keeps the domain message so operators can tell those conditions
// apart, while the 404 branch is answered with the id alone: this subtree is
// exempt from daemon authentication and the storage errors behind a missing
// sandbox embed absolute paths.
func jupyterEnsureReadyHTTPError(sessionID string, err error) error {
	if errors.Is(err, domain.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, missingSandboxMessage(sessionID))
	}
	return echo.NewHTTPError(http.StatusBadGateway, err.Error())
}

// missingSandboxMessage is the only sandbox detail this unauthenticated route
// discloses, so a storage path can never reach the response body.
func missingSandboxMessage(sessionID string) string {
	return "sandbox " + sessionID + " not found"
}

func newJupyterProxyTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}

func JupyterTargetReachable(proxyState domain.ProxyState, timeout time.Duration) bool {
	return sandboxes.JupyterTargetReachable(proxyState, timeout)
}
