package proto

//go:generate sh -c "protoc --go_out=. --go-grpc_out=. --proto_path=. platform.proto agent.proto && cp ebof-wg-mesh/api/proto/platformv1/*.pb.go platformv1/ && cp ebof-wg-mesh/api/proto/agentv1/*.pb.go agentv1/ && rm -rf ebof-wg-mesh"
