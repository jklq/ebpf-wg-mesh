package publicapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	protoMarshal   = protojson.MarshalOptions{UseProtoNames: true}
	protoUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: false}
)

// decodeBody reads a JSON request body into the RPC request message.
// Path parameters are applied by handlers afterwards and always win over
// body fields. An empty body decodes to the zero message.
func (s *Server) decodeBody(w http.ResponseWriter, r *http.Request, msg proto.Message) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody))
	if err != nil {
		return badRequest("request body too large or unreadable")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := protoUnmarshal.Unmarshal(body, msg); err != nil {
		return badRequest(fmt.Sprintf("invalid request body: %v", err))
	}
	return nil
}

// decodeRawFields decodes the body as a generic object for endpoints
// whose REST body is a small rename of RPC fields (confirmations, names).
func (s *Server) decodeRawFields(w http.ResponseWriter, r *http.Request, out any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody))
	if err != nil {
		return badRequest("request body too large or unreadable")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return badRequest(fmt.Sprintf("invalid request body: %v", err))
	}
	return nil
}

func (s *Server) writeProto(w http.ResponseWriter, r *http.Request, status int, msg proto.Message) {
	data, err := protoMarshal.Marshal(msg)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if id := requestID(r); id != "" {
		w.Header().Set("X-Request-Id", id)
	}
	w.WriteHeader(status)
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n"))
}

func queryBool(r *http.Request, name string) (bool, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, badRequest(fmt.Sprintf("query parameter %s must be true or false", name))
	}
	return value, nil
}

func queryInt32(r *http.Request, name string) (int32, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0, badRequest(fmt.Sprintf("query parameter %s must be an integer", name))
	}
	return int32(value), nil
}

func queryInt64(r *http.Request, name string) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, badRequest(fmt.Sprintf("query parameter %s must be an integer", name))
	}
	return value, nil
}

// queryTime parses an RFC3339 timestamp query parameter.
func queryTime(r *http.Request, name string) (*timestamppb.Timestamp, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, badRequest(fmt.Sprintf("query parameter %s must be an RFC3339 timestamp", name))
	}
	return timestamppb.New(parsed), nil
}
