package proxy

import (
	"errors"
	"log/slog"
	"net/url"

	protocolbridge "github.com/chaitin/ai-api-protocol-bridge"
	"github.com/labstack/echo/v4"

	"github.com/chaitin/agent-compose/pkg/llms"
)

// upstreamCall identifies the upstream call a failure is logged for. Model is
// the one requested, because the connection's configured model may be blank on
// a passthrough.
type upstreamCall struct {
	Target   llms.ResolvedTarget
	Model    string
	Protocol protocolbridge.Protocol
}

// logUpstreamFailure records why a proxied call answered 502. A guest that
// hung up mid-call cancels the upstream request too, and without the cause it
// is indistinguishable from an upstream that refused the connection, so the
// two are logged apart.
func (h runtimeLLMHandler) logUpstreamFailure(c echo.Context, msg string, call upstreamCall, err error) {
	ctx := c.Request().Context()
	attrs := []any{
		"sandbox_id", c.Param("sandbox_id"), "route", c.Path(),
		"connection", call.Target.Provider.ID, "model", call.Model,
		"upstream_protocol", string(call.Protocol),
	}
	attrs = append(attrs, upstreamErrorAttrs(err)...)
	if ctx.Err() != nil {
		h.logger().InfoContext(ctx, msg+": the guest disconnected", attrs...)
		return
	}
	h.logger().WarnContext(ctx, msg, attrs...)
}

// upstreamErrorAttrs describes a failed upstream call without the request URL's
// query. A transport failure is a *url.Error whose message carries the full URL,
// and net/http redacts only a userinfo password; a connection may keep a
// credential in its base URL's query, which NormalizeAPIBaseURL preserves. The
// URL is logged as scheme, host and path, and the error without the URL.
func upstreamErrorAttrs(err error) []any {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return []any{"error", err}
	}
	return []any{"upstream", redactedUpstreamURL(urlErr.URL), "error", urlErr.Err}
}

// redactedUpstreamURL keeps a URL's scheme, host and path. A URL that does not
// parse is dropped rather than logged, since nothing in it can be told apart
// from a credential.
func redactedUpstreamURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: parsed.Path}).String()
}

func (h runtimeLLMHandler) logger() *slog.Logger {
	if h.opts.Logger != nil {
		return h.opts.Logger
	}
	return slog.Default()
}
