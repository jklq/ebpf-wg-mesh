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
	createProjectFromSession,
	createServiceFastFromSession,
	deleteDomainBindingFromSession,
	discardServiceChangesFromSession,
	getServiceStatusFromSession,
	inspectRepositoryFromSession,
	listDomainBindingsFromSession,
	listServiceDeploymentsFromSession,
	listServiceLogsFromSession,
	loadDashboardHome,
	publishDomainFromSession,
	redeployServiceFromSession,
	saveHostnameFromSession,
	saveServicePositionFromSession,
	updateDomainBindingFromSession,
	updateServiceFromSession,
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
		loadDashboardHome() {
			return loadDashboardHome(runtime);
		},
		createProjectFromSession(name) {
			return createProjectFromSession(runtime, name);
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
		listServiceLogsFromSession(input) {
			return listServiceLogsFromSession(runtime, input);
		},
		listServiceDeploymentsFromSession(input) {
			return listServiceDeploymentsFromSession(runtime, input);
		},
		updateServiceFromSession(input) {
			return updateServiceFromSession(runtime, input);
		},
		redeployServiceFromSession(input) {
			return redeployServiceFromSession(runtime, input);
		},
		discardServiceChangesFromSession(input) {
			return discardServiceChangesFromSession(runtime, input);
		},
		saveServicePositionFromSession(input) {
			return saveServicePositionFromSession(runtime, input);
		},
		listDomainBindingsFromSession(input) {
			return listDomainBindingsFromSession(runtime, input);
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
