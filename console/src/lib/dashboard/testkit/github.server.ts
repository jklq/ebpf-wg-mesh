import type {
	GitHubAppUserClient,
	GitHubUserRepository,
} from "#/lib/dashboard/core/types.server";

export interface FakeGitHubAppUserClient extends GitHubAppUserClient {
	authorizationURLs: Array<{ redirectURI: string; state: string }>;
	exchangedCodes: Array<{ code: string; redirectURI: string }>;
	refreshedTokens: Array<string>;
	fetchedAccessTokens: Array<string>;
	listRepositoriesCalls: Array<string>;
	nextToken: {
		accessToken: string;
		tokenType: string;
		scope: string;
	};
	nextIdentity?:
		| {
				providerSubject: string;
				login: string;
				primaryEmail: string;
		  }
		| undefined;
	nextRepositories: Array<GitHubUserRepository>;
	identityError?: Error;
	listRepositoriesErrors: Array<Error>;
	listRepositoriesError?: Error;
}

export function createFakeGitHubAppUserClient(): FakeGitHubAppUserClient {
	const github: FakeGitHubAppUserClient = {
		authorizationURLs: [],
		exchangedCodes: [],
		refreshedTokens: [],
		fetchedAccessTokens: [],
		listRepositoriesCalls: [],
		nextToken: {
			accessToken: "github-access-token",
			tokenType: "bearer",
			scope: "read:user,user:email,repo",
		},
		nextIdentity: {
			providerSubject: "github-user-1",
			login: "octocat",
			primaryEmail: "user@example.com",
		},
		nextRepositories: [
			{
				owner: "octocat",
				name: "hello",
				fullName: "octocat/hello",
				private: false,
				defaultBranch: "main",
			},
		],
		listRepositoriesErrors: [],
		buildAuthorizationURL(input) {
			github.authorizationURLs.push(input);
			return `https://github.example.test/login/oauth/authorize?state=${encodeURIComponent(input.state)}`;
		},
		async exchangeCode(input) {
			github.exchangedCodes.push(input);
			return github.nextToken;
		},
		async refreshToken(refreshToken) {
			github.refreshedTokens.push(refreshToken);
			return github.nextToken;
		},
		async fetchIdentity(accessToken) {
			github.fetchedAccessTokens.push(accessToken);
			if (github.identityError) {
				throw github.identityError;
			}
			if (!github.nextIdentity) {
				throw new Error("missing fake GitHub identity");
			}
			return github.nextIdentity;
		},
		async listRepositories(accessToken) {
			github.listRepositoriesCalls.push(accessToken);
			const nextError = github.listRepositoriesErrors.shift();
			if (nextError) {
				throw nextError;
			}
			if (github.listRepositoriesError) {
				throw github.listRepositoriesError;
			}
			return github.nextRepositories;
		},
	};
	return github;
}
