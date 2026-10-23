package publicapi

import (
	"net/http"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func (s *Server) serviceRoutes() []routeEntry {
	env := scopeRule{kind: "environment", param: "environmentId"}
	envWrite := scopeRule{kind: "environment", param: "environmentId", write: true}
	svc := scopeRule{kind: "service", param: "serviceId"}
	svcWrite := scopeRule{kind: "service", param: "serviceId", write: true}
	dom := scopeRule{kind: "domain", param: "hostname"}
	domWrite := scopeRule{kind: "domain", param: "hostname", write: true}
	vol := scopeRule{kind: "volume", param: "volumeId"}
	volWrite := scopeRule{kind: "volume", param: "volumeId", write: true}
	return []routeEntry{
		{method: "GET", pattern: "/v1/environments/{environmentId}/services", summary: "List services and volumes in an environment (include_deleted)", rule: env, resp: "platform.v1.ListServicesResponse", handler: func(w http.ResponseWriter, r *http.Request) {
			includeDeleted, err := queryBool(r, "include_deleted")
			if err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.ListServices(r.Context(), &platformv1.ListServicesRequest{EnvironmentId: r.PathValue("environmentId"), IncludeDeleted: includeDeleted})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/environments/{environmentId}/services", summary: "Create a service (staged until the environment is released)", rule: envWrite, req: "platform.v1.CreateServiceRequest", resp: "platform.v1.Service", handler: func(w http.ResponseWriter, r *http.Request) {
			req := &platformv1.CreateServiceRequest{}
			if err := s.decodeBody(w, r, req); err != nil {
				s.writeError(w, r, err)
				return
			}
			req.EnvironmentId = r.PathValue("environmentId")
			resp, err := s.platform.CreateService(r.Context(), req)
			s.respond(w, r, http.StatusCreated, resp, err)
		}},
		{method: "GET", pattern: "/v1/services/{serviceId}", summary: "Get a service (desired spec, source, latest build and deployment; env values redacted unless include_secrets)", rule: svc, resp: "platform.v1.Service", handler: func(w http.ResponseWriter, r *http.Request) {
			includeSecrets, err := queryBool(r, "include_secrets")
			if err != nil {
				s.writeError(w, r, err)
				return
			}
			// Revealing secrets is a write-tier capability: a read-access
			// token must not exfiltrate env values even when its user could.
			if includeSecrets {
				if token, ok := tokenFromContext(r.Context()); ok && token.Access == AccessRead {
					s.writeError(w, r, forbiddenScope("secret values require a full-access token"))
					return
				}
			}
			resp, err := s.platform.GetService(r.Context(), &platformv1.GetServiceRequest{ServiceId: r.PathValue("serviceId"), IncludeSecrets: includeSecrets})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "PUT", pattern: "/v1/services/{serviceId}", summary: "Replace a service draft (expected_spec_revision must be current; staged until release)", rule: svcWrite, req: "platform.v1.UpdateServiceRequest", resp: "platform.v1.Service", handler: func(w http.ResponseWriter, r *http.Request) {
			req := &platformv1.UpdateServiceRequest{}
			if err := s.decodeBody(w, r, req); err != nil {
				s.writeError(w, r, err)
				return
			}
			req.ServiceId = r.PathValue("serviceId")
			resp, err := s.platform.UpdateService(r.Context(), req)
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "DELETE", pattern: "/v1/services/{serviceId}", summary: "Delete a service (restorable during grace)", rule: svcWrite, handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.DeleteService(r.Context(), &platformv1.DeleteServiceRequest{ServiceId: r.PathValue("serviceId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/services/{serviceId}/restore", summary: "Restore a deleted service", rule: svcWrite, resp: "platform.v1.Service", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.RestoreService(r.Context(), &platformv1.RestoreServiceRequest{ServiceId: r.PathValue("serviceId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "GET", pattern: "/v1/services/{serviceId}/status", summary: "Service with live allocation status", rule: svc, resp: "platform.v1.ServiceStatus", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.GetServiceStatus(r.Context(), &platformv1.GetServiceStatusRequest{ServiceId: r.PathValue("serviceId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/services/{serviceId}/scale", summary: "Set the desired replica count", rule: svcWrite, resp: "platform.v1.ServiceStatus", handler: func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				DesiredReplicaCount int32 `json:"desired_replica_count"`
			}
			if err := s.decodeRawFields(w, r, &body); err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.ScaleService(r.Context(), &platformv1.ScaleServiceRequest{ServiceId: r.PathValue("serviceId"), DesiredReplicaCount: body.DesiredReplicaCount})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/services/{serviceId}/discard-changes", summary: "Discard staged service changes (change_ids or discard_all)", rule: svcWrite, req: "platform.v1.DiscardServiceChangesRequest", resp: "platform.v1.Service", handler: func(w http.ResponseWriter, r *http.Request) {
			req := &platformv1.DiscardServiceChangesRequest{}
			if err := s.decodeBody(w, r, req); err != nil {
				s.writeError(w, r, err)
				return
			}
			req.ServiceId = r.PathValue("serviceId")
			resp, err := s.platform.DiscardServiceChanges(r.Context(), req)
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "GET", pattern: "/v1/services/{serviceId}/deployments", summary: "List deployments, newest first (limit)", rule: svc, resp: "platform.v1.ListServiceDeploymentsResponse", handler: func(w http.ResponseWriter, r *http.Request) {
			limit, err := queryInt32(r, "limit")
			if err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.ListServiceDeployments(r.Context(), &platformv1.ListServiceDeploymentsRequest{ServiceId: r.PathValue("serviceId"), Limit: limit})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/services/{serviceId}/deployments/{deploymentId}/actions", summary: "Act on a deployment: restart, exact redeploy, rollback, cancel, remove, retry", rule: svcWrite, req: "platform.v1.ApplyDeploymentActionRequest", resp: "platform.v1.ServiceStatus", handler: func(w http.ResponseWriter, r *http.Request) {
			req := &platformv1.ApplyDeploymentActionRequest{}
			if err := s.decodeBody(w, r, req); err != nil {
				s.writeError(w, r, err)
				return
			}
			req.ServiceId, req.DeploymentId = r.PathValue("serviceId"), r.PathValue("deploymentId")
			resp, err := s.platform.ApplyDeploymentAction(r.Context(), req)
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "GET", pattern: "/v1/services/{serviceId}/logs", summary: "Service logs (log_type, limit, search, times, page tokens)", rule: svc, resp: "platform.v1.ListServiceLogsResponse", handler: func(w http.ResponseWriter, r *http.Request) {
			req, err := logsRequest(r)
			if err != nil {
				s.writeError(w, r, err)
				return
			}
			req.ServiceId = r.PathValue("serviceId")
			resp, err := s.platform.ListServiceLogs(r.Context(), req)
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "GET", pattern: "/v1/services/{serviceId}/artifacts", summary: "List built image artifacts (limit)", rule: svc, resp: "platform.v1.ListServiceArtifactsResponse", handler: func(w http.ResponseWriter, r *http.Request) {
			limit, err := queryInt32(r, "limit")
			if err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.ListServiceArtifacts(r.Context(), &platformv1.ListServiceArtifactsRequest{ServiceId: r.PathValue("serviceId"), Limit: limit})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "GET", pattern: "/v1/services/{serviceId}/builds/{buildId}/attempts", summary: "List attempts of one build", rule: svc, resp: "platform.v1.ListBuildAttemptsResponse", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.ListBuildAttempts(r.Context(), &platformv1.ListBuildAttemptsRequest{ServiceId: r.PathValue("serviceId"), BuildId: r.PathValue("buildId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "GET", pattern: "/v1/services/{serviceId}/domains", summary: "List domain bindings of a service (include_deleted)", rule: svc, resp: "platform.v1.ListDomainBindingsResponse", handler: func(w http.ResponseWriter, r *http.Request) {
			includeDeleted, err := queryBool(r, "include_deleted")
			if err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.ListDomainBindings(r.Context(), &platformv1.ListDomainBindingsRequest{ServiceId: r.PathValue("serviceId"), IncludeDeleted: includeDeleted})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/services/{serviceId}/domains", summary: "Bind a custom hostname to a service port", rule: svcWrite, req: "platform.v1.DomainBindingInput", resp: "platform.v1.DomainBinding", handler: func(w http.ResponseWriter, r *http.Request) {
			input := &platformv1.DomainBindingInput{}
			if err := s.decodeBody(w, r, input); err != nil {
				s.writeError(w, r, err)
				return
			}
			input.ServiceId = r.PathValue("serviceId")
			resp, err := s.platform.CreateDomainBinding(r.Context(), &platformv1.CreateDomainBindingRequest{Binding: input})
			s.respond(w, r, http.StatusCreated, resp, err)
		}},
		{method: "POST", pattern: "/v1/services/{serviceId}/generated-domain", summary: "Generate a platform hostname for a service port", rule: svcWrite, req: "platform.v1.GenerateDomainBindingRequest", resp: "platform.v1.DomainBinding", handler: func(w http.ResponseWriter, r *http.Request) {
			req := &platformv1.GenerateDomainBindingRequest{}
			if err := s.decodeBody(w, r, req); err != nil {
				s.writeError(w, r, err)
				return
			}
			req.ServiceId = r.PathValue("serviceId")
			resp, err := s.platform.GenerateDomainBinding(r.Context(), req)
			s.respond(w, r, http.StatusCreated, resp, err)
		}},
		{method: "GET", pattern: "/v1/domains/{hostname}", summary: "Get a domain binding, with ownership and certificate state", rule: dom, resp: "platform.v1.DomainBinding", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.GetDomainBinding(r.Context(), &platformv1.GetDomainBindingRequest{Hostname: r.PathValue("hostname")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "PATCH", pattern: "/v1/domains/{hostname}", summary: "Retarget a domain binding to a service port", rule: domWrite, req: "platform.v1.DomainBindingTarget", resp: "platform.v1.DomainBinding", handler: func(w http.ResponseWriter, r *http.Request) {
			token, _ := tokenFromContext(r.Context())
			target := &platformv1.DomainBindingTarget{}
			if err := s.decodeBody(w, r, target); err != nil {
				s.writeError(w, r, err)
				return
			}
			if err := s.checkResourceScope(r.Context(), token, "service", target.GetServiceId()); err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.UpdateDomainBinding(r.Context(), &platformv1.UpdateDomainBindingRequest{Hostname: r.PathValue("hostname"), Binding: target})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "DELETE", pattern: "/v1/domains/{hostname}", summary: "Delete a domain binding (restorable during grace)", rule: domWrite, handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.DeleteDomainBinding(r.Context(), &platformv1.DeleteDomainBindingRequest{Hostname: r.PathValue("hostname")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/domains/{hostname}/restore", summary: "Restore a deleted domain binding", rule: domWrite, resp: "platform.v1.DomainBinding", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.RestoreDomainBinding(r.Context(), &platformv1.RestoreDomainBindingRequest{Hostname: r.PathValue("hostname")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "GET", pattern: "/v1/environments/{environmentId}/volumes", summary: "List volumes in an environment (include_deleted)", rule: env, resp: "platform.v1.ListVolumesResponse", handler: func(w http.ResponseWriter, r *http.Request) {
			includeDeleted, err := queryBool(r, "include_deleted")
			if err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.ListVolumes(r.Context(), &platformv1.ListVolumesRequest{EnvironmentId: r.PathValue("environmentId"), IncludeDeleted: includeDeleted})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/environments/{environmentId}/volumes", summary: "Create a volume (staged until the environment is released)", rule: envWrite, req: "platform.v1.CreateVolumeRequest", resp: "platform.v1.Volume", handler: func(w http.ResponseWriter, r *http.Request) {
			req := &platformv1.CreateVolumeRequest{}
			if err := s.decodeBody(w, r, req); err != nil {
				s.writeError(w, r, err)
				return
			}
			req.EnvironmentId = r.PathValue("environmentId")
			resp, err := s.platform.CreateVolume(r.Context(), req)
			s.respond(w, r, http.StatusCreated, resp, err)
		}},
		{method: "GET", pattern: "/v1/volumes/{volumeId}", summary: "Get one volume by ID", rule: vol, resp: "platform.v1.Volume", handler: func(w http.ResponseWriter, r *http.Request) {
			// The platform has no get-volume RPC; resolve the owning
			// environment (already scope-checked) and pick from its list.
			scope, found, err := s.scopes.Resolve(r.Context(), "volume", r.PathValue("volumeId"))
			if err != nil {
				s.writeError(w, r, err)
				return
			}
			if !found {
				s.writeError(w, r, &httpError{status: http.StatusNotFound, code: "not_found", message: "volume not found"})
				return
			}
			listed, err := s.platform.ListVolumes(r.Context(), &platformv1.ListVolumesRequest{EnvironmentId: scope.EnvironmentID, IncludeDeleted: true})
			if err != nil {
				s.writeError(w, r, err)
				return
			}
			for _, volume := range listed.GetVolumes() {
				if volume.GetId() == r.PathValue("volumeId") {
					s.respond(w, r, http.StatusOK, volume, nil)
					return
				}
			}
			s.writeError(w, r, &httpError{status: http.StatusNotFound, code: "not_found", message: "volume not found"})
		}},
		{method: "PATCH", pattern: "/v1/volumes/{volumeId}", summary: "Grow a volume (shrinking is rejected)", rule: volWrite, resp: "platform.v1.Volume", handler: func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				SizeBytes int64 `json:"size_bytes"`
			}
			if err := s.decodeRawFields(w, r, &body); err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.UpdateVolume(r.Context(), &platformv1.UpdateVolumeRequest{VolumeId: r.PathValue("volumeId"), SizeBytes: body.SizeBytes})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "DELETE", pattern: "/v1/volumes/{volumeId}", summary: "Delete a volume (confirmation_name must match)", rule: volWrite, handler: func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				ConfirmationName string `json:"confirmation_name"`
			}
			if err := s.decodeRawFields(w, r, &body); err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.DeleteVolume(r.Context(), &platformv1.DeleteVolumeRequest{VolumeId: r.PathValue("volumeId"), ConfirmationName: body.ConfirmationName})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "GET", pattern: "/v1/volumes/{volumeId}/deletion-preview", summary: "Preview what deleting the volume affects", rule: vol, resp: "platform.v1.DeletionPreview", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.PreviewVolumeDeletion(r.Context(), &platformv1.PreviewVolumeDeletionRequest{VolumeId: r.PathValue("volumeId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
	}
}

// logsRequest builds a ListServiceLogsRequest from query parameters.
// log_type accepts friendly names (runtime, build, deploy, http, network)
// or full enum names (SERVICE_LOG_TYPE_RUNTIME).
func logsRequest(r *http.Request) (*platformv1.ListServiceLogsRequest, error) {
	limit, err := queryInt32(r, "limit")
	if err != nil {
		return nil, err
	}
	start, err := queryTime(r, "start_time")
	if err != nil {
		return nil, err
	}
	end, err := queryTime(r, "end_time")
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	req := &platformv1.ListServiceLogsRequest{
		AllocationId: q.Get("allocation_id"), BuildId: q.Get("build_id"),
		Search: q.Get("search"), PageToken: q.Get("page_token"), GapPageToken: q.Get("gap_page_token"),
		Limit: limit, StartTime: start, EndTime: end,
	}
	if raw := q.Get("log_type"); raw != "" {
		value, ok := platformv1.ServiceLogType_value[raw]
		if !ok {
			value, ok = platformv1.ServiceLogType_value["SERVICE_LOG_TYPE_"+asciiUpper(raw)]
		}
		if !ok {
			return nil, badRequest("query parameter log_type must be runtime, build, deploy, http, or network")
		}
		req.LogType = platformv1.ServiceLogType(value)
	}
	return req, nil
}
