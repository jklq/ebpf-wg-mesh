package publicapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// apiError is the single error shape of the public API:
//
//	{"error": {"code": "not_found", "message": "...", "request_id": "..."}}
type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

type errorBody struct {
	Error apiError `json:"error"`
}

// httpError carries an explicit HTTP status through handler code.
type httpError struct {
	status  int
	code    string
	message string
}

func (e *httpError) Error() string { return e.message }

func badRequest(message string) error {
	return &httpError{status: http.StatusBadRequest, code: "invalid_argument", message: message}
}

func forbiddenScope(message string) error {
	return &httpError{status: http.StatusForbidden, code: "permission_denied", message: message}
}

func unauthorized(message string) error {
	return &httpError{status: http.StatusUnauthorized, code: "unauthenticated", message: message}
}

// statusForCode maps control-plane gRPC codes to HTTP status and the
// public error code vocabulary.
func statusForCode(code codes.Code) (int, string) {
	switch code {
	case codes.InvalidArgument:
		return http.StatusBadRequest, "invalid_argument"
	case codes.Unauthenticated:
		return http.StatusUnauthorized, "unauthenticated"
	case codes.PermissionDenied:
		return http.StatusForbidden, "permission_denied"
	case codes.NotFound:
		return http.StatusNotFound, "not_found"
	case codes.AlreadyExists, codes.Aborted:
		return http.StatusConflict, "conflict"
	case codes.FailedPrecondition:
		return http.StatusConflict, "failed_precondition"
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests, "rate_limited"
	case codes.Unavailable:
		return http.StatusServiceUnavailable, "unavailable"
	case codes.DeadlineExceeded, codes.Canceled:
		return http.StatusGatewayTimeout, "deadline_exceeded"
	case codes.Unimplemented:
		return http.StatusNotImplemented, "unimplemented"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

type requestIDContextKey struct{}

func requestID(r *http.Request) string {
	if id, ok := r.Context().Value(requestIDContextKey{}).(string); ok {
		return id
	}
	return ""
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		httpStatus int
		code       string
		message    string
	)
	var he *httpError
	switch {
	case errors.As(err, &he):
		httpStatus, code, message = he.status, he.code, he.message
	case errors.Is(err, ErrInvalidToken), errors.Is(err, ErrExpiredToken), errors.Is(err, ErrRevokedToken):
		httpStatus, code, message = http.StatusUnauthorized, "unauthenticated", err.Error()
	default:
		httpStatus, code = statusForCode(status.Code(err))
		message = status.Convert(err).Message()
		if message == "" {
			message = err.Error()
		}
		// Internal errors keep their detail in server logs, not responses.
		if httpStatus == http.StatusInternalServerError {
			message = "internal error"
		}
	}
	s.writeJSON(w, r, httpStatus, errorBody{Error: apiError{
		Code: code, Message: message, RequestID: requestID(r),
	}})
}

func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	if id := requestID(r); id != "" {
		w.Header().Set("X-Request-Id", id)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func bearerSecret(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	secret, found := strings.CutPrefix(header, "Bearer ")
	if !found {
		secret, found = strings.CutPrefix(header, "bearer ")
	}
	secret = strings.TrimSpace(secret)
	return secret, found && secret != ""
}
