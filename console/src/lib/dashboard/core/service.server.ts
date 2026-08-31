import {
	beginGitHubLogin,
	clearSession,
	completeAuthCallback,
	refreshSession,
} from "#/lib/dashboard/core/auth.server";
import {
	checkDomainDNSFromSession,
	confirmRepositoryFromSession,
	createDomainBindingFromSession,
	createEnvironmentFromSession,
	createProjectFromSession,
	createServiceFastFromSession,
	deleteDomainBindingFromSession,
	deleteEnvironmentFromSession,
	deleteServiceFromSession,
	deployEnvironmentFromSession,
	discardServiceChangesFromSession,
	duplicateEnvironmentFromSession,
	generateDomainBindingFromSession,
	getServiceStatusFromSession,
	inspectRepositoryFromSession,
	inspectRepositorySourceFromSession,
	loadFleetFromSession,
	listDomainBindingsFromSession,
	listEnvironmentServicesFromSession,
	listServiceDeploymentsFromSession,
	listServiceLogsFromSession,
	loadDashboardHome,
	loadGitHubCatalogFromSession,
	publishDomainFromSession,
	applyDeploymentActionFromSession,
	scaleServiceFromSession,
	renameEnvironmentFromSession,
	saveHostnameFromSession,
	saveServicePositionFromSession,
	updateDomainBindingFromSession,
	createFleetAgentFromSession,
	updateFleetAgentFromSession,
	setFleetAgentLifecycleFromSession,
	updateServiceFromSession,
	waitForEnvironmentServicesFromSession,
	waitForProjectServicesFromSession,
	waitForServiceStatusFromSession,
} from "#/lib/dashboard/core/operations.server";
import { createDashboardRuntime } from "#/lib/dashboard/core/runtime.server";
import type {
	DashboardConfig,
	DashboardDependencies,
	DashboardService,
} from "#/lib/dashboard/core/types.server";

export function createDashboardService(
	config: DashboardConfig,
	deps: DashboardDependencies,
): DashboardService {
	const runtime = createDashboardRuntime(config, deps);

	return {
		loadFleetFromSession() {
			return loadFleetFromSession(runtime);
		},
		createFleetAgentFromSession(input) {
			return createFleetAgentFromSession(runtime, input);
		},
		updateFleetAgentFromSession(input) {
			return updateFleetAgentFromSession(runtime, input);
		},
		setFleetAgentLifecycleFromSession(input) {
			return setFleetAgentLifecycleFromSession(runtime, input);
		},
		listDevLogins() {
			return config.devUsers;
		},
		isGitHubLoginEnabled() {
			return Boolean(config.github && runtime.github);
		},
		getPublicBaseURL() {
			return config.publicBaseURL;
		},
		beginGitHubLogin(input) {
			return beginGitHubLogin(runtime, input);
		},
		completeAuthCallback(input) {
			return completeAuthCallback(runtime, input);
		},
		loadDashboardHome(environmentId) {
			return loadDashboardHome(runtime, environmentId);
		},
		loadGitHubCatalogFromSession() {
			return loadGitHubCatalogFromSession(runtime);
		},
		inspectRepositorySourceFromSession(input) {
			return inspectRepositorySourceFromSession(runtime, input);
		},
		createProjectFromSession(name) {
			return createProjectFromSession(runtime, name);
		},
		createEnvironmentFromSession(input) {
			return createEnvironmentFromSession(runtime, input);
		},
		duplicateEnvironmentFromSession(input) {
			return duplicateEnvironmentFromSession(runtime, input);
		},
		renameEnvironmentFromSession(input) {
			return renameEnvironmentFromSession(runtime, input);
		},
		deleteEnvironmentFromSession(environmentId) {
			return deleteEnvironmentFromSession(runtime, environmentId);
		},
		deployEnvironmentFromSession(environmentId) {
			return deployEnvironmentFromSession(runtime, environmentId);
		},
		inspectRepositoryFromSession(input) {
			return inspectRepositoryFromSession(runtime, input);
		},
		confirmRepositoryFromSession(input) {
			return confirmRepositoryFromSession(runtime, input);
		},
		createServiceFastFromSession(input) {
			return createServiceFastFromSession(runtime, input);
		},
		saveHostnameFromSession(hostname) {
			return saveHostnameFromSession(runtime, hostname);
		},
		publishDomainFromSession() {
			return publishDomainFromSession(runtime);
		},
		clearSession() {
			return clearSession(runtime);
		},
		refreshSession() {
			return refreshSession(runtime);
		},
		getServiceStatusFromSession(input) {
			return getServiceStatusFromSession(runtime, input);
		},
		listEnvironmentServicesFromSession(input) {
			return listEnvironmentServicesFromSession(runtime, input);
		},
		waitForEnvironmentServicesFromSession(input) {
			return waitForEnvironmentServicesFromSession(runtime, input);
		},
		waitForProjectServicesFromSession(input) {
			return waitForProjectServicesFromSession(runtime, input);
		},
		waitForServiceStatusFromSession(input) {
			return waitForServiceStatusFromSession(runtime, input);
		},
		listServiceLogsFromSession(input) {
			return listServiceLogsFromSession(runtime, input);
		},
		listServiceDeploymentsFromSession(input) {
			return listServiceDeploymentsFromSession(runtime, input);
		},
		updateServiceFromSession(input) {
			return updateServiceFromSession(runtime, input);
		},
		applyDeploymentActionFromSession(input) {
			return applyDeploymentActionFromSession(runtime, input);
		},
		scaleServiceFromSession(input) {
			return scaleServiceFromSession(runtime, input);
		},
		discardServiceChangesFromSession(input) {
			return discardServiceChangesFromSession(runtime, input);
		},
		deleteServiceFromSession(input) {
			return deleteServiceFromSession(runtime, input);
		},
		saveServicePositionFromSession(input) {
			return saveServicePositionFromSession(runtime, input);
		},
		listDomainBindingsFromSession(input) {
			return listDomainBindingsFromSession(runtime, input);
		},
		generateDomainBindingFromSession(input) {
			return generateDomainBindingFromSession(runtime, input);
		},
		createDomainBindingFromSession(input) {
			return createDomainBindingFromSession(runtime, input);
		},
		updateDomainBindingFromSession(input) {
			return updateDomainBindingFromSession(runtime, input);
		},
		deleteDomainBindingFromSession(input) {
			return deleteDomainBindingFromSession(runtime, input);
		},
		checkDomainDNSFromSession(hostname) {
			return checkDomainDNSFromSession(runtime, hostname);
		},
	};
}
