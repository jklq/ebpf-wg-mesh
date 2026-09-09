package delivery

import (
	"encoding/json"
	"fmt"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/encoding/protojson"
)

type jsonInt32Slice []int32

type JSONInt32Slice = jsonInt32Slice

type jsonStringSlice []string

func encodeHealthyPorts(ports []int32) ([]byte, error) {
	if ports == nil {
		ports = []int32{}
	}
	return json.Marshal(ports)
}

func encodeRestartObservation(obs *platformv1.RestartObservation) ([]byte, error) {
	if obs == nil {
		return []byte("{}"), nil
	}
	return protojson.Marshal(obs)
}

func decodeRestartObservation(raw []byte) (*platformv1.RestartObservation, error) {
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return nil, nil
	}
	obs := &platformv1.RestartObservation{}
	if err := protojson.Unmarshal(raw, obs); err != nil {
		return nil, err
	}
	return obs, nil
}

func (p *jsonStringSlice) Scan(src any) error {
	if p == nil {
		return nil
	}
	switch v := src.(type) {
	case nil:
		*p = nil
		return nil
	case []byte:
		return json.Unmarshal(v, (*[]string)(p))
	case string:
		return json.Unmarshal([]byte(v), (*[]string)(p))
	default:
		return fmt.Errorf("scan string slice json: unsupported type %T", src)
	}
}

func (p *jsonInt32Slice) Scan(src any) error {
	if p == nil {
		return nil
	}
	switch v := src.(type) {
	case nil:
		*p = nil
		return nil
	case []byte:
		return json.Unmarshal(v, (*[]int32)(p))
	case string:
		return json.Unmarshal([]byte(v), (*[]int32)(p))
	default:
		return fmt.Errorf("scan int32 slice json: unsupported type %T", src)
	}
}
