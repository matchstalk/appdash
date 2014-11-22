package httptrace

import (
	"log"
	"net/http"
	"time"

	"sourcegraph.com/sourcegraph/apptrace"
)

// NewServerEvent returns an event which records various aspects of an
// HTTP response. It takes an HTTP request, not response, as input
// because the information it records is derived from the request, and
// HTTP handlers don't have access to the response struct (only
// http.ResponseWriter, which requires wrapping or buffering to
// introspect).
//
// The returned value is incomplete and should have its Response and
// ServerRecv/ServerSend values set before being logged.
func NewServerEvent(r *http.Request) *ServerEvent {
	return &ServerEvent{Request: requestInfo(r)}
}

// ResponseInfo describes an HTTP response.
type ResponseInfo struct {
	Headers       map[string]string
	ContentLength int64
	StatusCode    int
}

func responseInfo(r *http.Response) ResponseInfo {
	return ResponseInfo{
		Headers:       redactHeaders(r.Header, r.Trailer),
		ContentLength: r.ContentLength,
		StatusCode:    r.StatusCode,
	}
}

// ServerEvent records an HTTP server request handling event.
type ServerEvent struct {
	Request    RequestInfo
	Response   ResponseInfo
	Route      string
	User       string
	ServerRecv time.Time
	ServerSend time.Time
}

// Schema returns the constant "HTTPServer".
func (ServerEvent) Schema() string { return "HTTPServer" }

// Middleware creates a new http.Handler middleware
// (negroni-compliant) that records incoming HTTP requests to the
// collector c as "HTTPServer"-schema events.
func Middleware(c apptrace.Collector, conf *MiddlewareConfig) func(rw http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	return func(rw http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
		spanID, err := GetSpanIDHeader(r.Header)
		if err != nil {
			log.Printf("Warning: invalid Span-ID header: %s. (Continuing with request handling.)", err)
		}
		setSpanIDFromClient := (spanID != nil)
		if spanID == nil {
			newSpanID := apptrace.NewRootSpanID()
			spanID = &newSpanID
		}

		if conf.SetContextSpan != nil {
			conf.SetContextSpan(r, *spanID)
		}

		e := NewServerEvent(r)
		e.ServerRecv = time.Now()
		if conf.RouteName != nil {
			e.Route = conf.RouteName(r)
		}
		if conf.CurrentUser != nil {
			e.User = conf.CurrentUser(r)
		}

		rr := &responseInfoRecorder{ResponseWriter: rw}
		next(rr, r)
		SetSpanIDHeader(rr.Header(), *spanID)

		if !setSpanIDFromClient {
			e.Request = requestInfo(r)
			log.Printf("e.Request = %+v", e.Request)
			log.Printf("e.Response = %+v", responseInfo(rr.partialResponse()))
		}
		e.Response = responseInfo(rr.partialResponse())
		e.ServerSend = time.Now()

		rec := apptrace.NewRecorder(*spanID, c)
		if e.Route != "" {
			rec.Name(e.Route)
		} else {
			rec.Name(e.Request.URI)
		}
		rec.Event(e)
	}
}

// MiddlewareConfig configures the HTTP tracing middleware.
type MiddlewareConfig struct {
	// RouteName, if non-nil, is called to get the current route's
	// name. This name is used as the span's name.
	RouteName func(*http.Request) string

	// CurrentUser, if non-nil, is called to get the current user ID
	// (which may be a login or a numeric ID).
	CurrentUser func(*http.Request) string

	// SetContextSpan, if non-nil, is called to set the span (which is
	// either taken from the client request header or created anew) in
	// the HTTP request context, so it may be used by other parts of
	// the handling process.
	SetContextSpan func(*http.Request, apptrace.SpanID)
}

// responseInfoRecorder is an http.ResponseWriter that records a
// response's HTTP status code and body length and forwards all
// operations onto an underlying http.ResponseWriter, without
// buffering the response body.
type responseInfoRecorder struct {
	statusCode    int   // HTTP response status code
	ContentLength int64 // number of bytes written using the Write method

	http.ResponseWriter // underlying ResponseWriter to pass-thru to
}

// Write always succeeds and writes to r.Body, if not nil.
func (r *responseInfoRecorder) Write(b []byte) (int, error) {
	r.ContentLength += int64(len(b))
	if r.statusCode == 0 {
		r.statusCode = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *responseInfoRecorder) StatusCode() int {
	if r.statusCode == 0 {
		return http.StatusOK
	}
	return r.statusCode
}

// WriteHeader sets r.Code.
func (r *responseInfoRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

// partialResponse constructs a partial response object based on the
// information it is able to determine about the response.
func (r *responseInfoRecorder) partialResponse() *http.Response {
	return &http.Response{
		StatusCode:    r.StatusCode(),
		ContentLength: r.ContentLength,
		Header:        r.Header(),
	}
}