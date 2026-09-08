export type AuthConflictCode =
	| "missing_verified_email"
	| "email_linked_to_other_github"
	| "ambiguous_existing_user"
	| "invalid_signin_state"
	| "github_auth_unavailable"
	| "dev_auth_unavailable"
	| "invalid_dev_login";

class DashboardTaggedError extends Error {
	readonly _tag: string;

	constructor(tag: string, message: string) {
		super(message);
		this.name = tag;
		this._tag = tag;
	}
}

export class AuthConflictError extends DashboardTaggedError {
	readonly code: AuthConflictCode;

	constructor(input: { code: AuthConflictCode; message: string }) {
		super("AuthConflictError", input.message);
		this.code = input.code;
	}
}

export class AuthenticationRequiredError extends DashboardTaggedError {
	constructor(input: { message: string }) {
		super("AuthenticationRequiredError", input.message);
	}
}

export class DashboardValidationError extends DashboardTaggedError {
	constructor(input: { message: string }) {
		super("DashboardValidationError", input.message);
	}
}

export class DashboardConfigError extends DashboardTaggedError {
	constructor(input: { message: string }) {
		super("DashboardConfigError", input.message);
	}
}

export class DatabaseError extends DashboardTaggedError {
	readonly operation: string;
	readonly cause: unknown;

	constructor(input: { operation: string; message: string; cause: unknown }) {
		super("DatabaseError", input.message);
		this.operation = input.operation;
		this.cause = input.cause;
	}
}

export class GitHubApiError extends DashboardTaggedError {
	readonly operation: string;
	readonly cause: unknown;
	readonly status?: number;

	constructor(input: {
		operation: string;
		message: string;
		cause: unknown;
		status?: number;
	}) {
		super("GitHubApiError", input.message);
		this.operation = input.operation;
		this.cause = input.cause;
		this.status = input.status;
	}
}

export class PlatformGatewayError extends DashboardTaggedError {
	readonly operation: string;
	readonly cause: unknown;
	readonly grpcCode?: number;

	constructor(input: {
		operation: string;
		message: string;
		cause: unknown;
		grpcCode?: number;
	}) {
		super("PlatformGatewayError", input.message);
		this.operation = input.operation;
		this.cause = input.cause;
		this.grpcCode = input.grpcCode;
	}
}
