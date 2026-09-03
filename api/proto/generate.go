package proto

//go:generate sh -c "buf generate && cp ebof-wg-mesh/api/proto/platformv1/*.go platformv1/ && mkdir -p platformv1connect && cp ebof-wg-mesh/api/proto/platformv1/platformv1connect/*.go platformv1connect/ && cp ebof-wg-mesh/api/proto/agentv1/*.go agentv1/ && rm -rf ebof-wg-mesh"
