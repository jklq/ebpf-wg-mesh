import { Buffer } from 'node:buffer'
import { randomUUID } from 'node:crypto'
import { dirname, resolve } from 'node:path'
import * as grpc from '@grpc/grpc-js'
import * as protoLoader from '@grpc/proto-loader'
import { deleteCookie, getCookie, setCookie } from '@tanstack/react-start/server'
import googleProtoFiles from 'google-proto-files'
import { Pool } from 'pg'

import {
  createDashboardService,
  parseDevUsers,
  parseIdentifier,
  type DashboardConfig,
  type DashboardProject,
  type DashboardStore,
  type DashboardUser,
  type DevLoginIdentity,
  type PlatformGateway,
} from '#/lib/dashboard-core.server'

interface RuntimeConfig extends DashboardConfig {
  databaseURL: string
  databaseSchema: string
  controlPlaneAddress: string
  controlPlaneServerName: string
  controlPlaneCA: Buffer
  controlPlaneCert: Buffer
  controlPlaneKey: Buffer
}

interface DashboardSessionRecord {
  sessionId: string
  user: DashboardUser
}

type PlatformClient = grpc.Client & {
  EnsurePrincipal: (
    request: Record<string, unknown>,
    metadata: grpc.Metadata,
    callback: (
      error: grpc.ServiceError | null,
      response: Record<string, unknown>,
    ) => void,
  ) => void
  ListProjects: (
    request: Record<string, unknown>,
    metadata: grpc.Metadata,
    callback: (
      error: grpc.ServiceError | null,
      response: Record<string, unknown>,
    ) => void,
  ) => void
  CreateProject: (
    request: Record<string, unknown>,
    metadata: grpc.Metadata,
    callback: (
      error: grpc.ServiceError | null,
      response: Record<string, unknown>,
    ) => void,
  ) => void
}

const config = readConfig()
const pool = new Pool({
  connectionString: config.databaseURL,
})

let initPromise: Promise<void> | undefined
let clientInstance: PlatformClient | undefined

const service = createDashboardService(config, {
  store: createPostgresDashboardStore(config, pool),
  platform: createPlatformGateway(config),
  cookies: {
    get: (name) => getCookie(name) ?? undefined,
    set: (name, value, options) => setCookie(name, value, options),
    delete: (name, options) => deleteCookie(name, options),
  },
  randomUUID,
})

export const listDevLogins = service.listDevLogins
export const completeDevLogin = service.completeDevLogin
export const loadDashboardHome = service.loadDashboardHome
export const createProjectFromSession = service.createProjectFromSession
export const clearSession = service.clearSession

function createPostgresDashboardStore(
  runtime: RuntimeConfig,
  db: Pool,
): DashboardStore {
  return {
    ensureInitialized,
    async upsertUser(subject, email): Promise<DashboardUser> {
      const id = randomUUID()
      const result = await db.query<{
        id: string
        subject: string
        email: string
      }>(
        `INSERT INTO ${tableName(runtime, 'users')} (id, subject, email, created_at, updated_at)
         VALUES ($1, $2, $3, NOW(), NOW())
         ON CONFLICT (subject) DO UPDATE SET email = excluded.email, updated_at = NOW()
         RETURNING id, subject, email`,
        [id, subject, email],
      )
      const user = result.rows[0]
      await db.query(
        `INSERT INTO ${tableName(runtime, 'accounts')} (id, user_id, provider, provider_subject, created_at)
         VALUES ($1, $2, $3, $4, NOW())
         ON CONFLICT (provider, provider_subject) DO NOTHING`,
        [randomUUID(), user.id, 'dev', user.subject],
      )
      await db.query(
        `INSERT INTO ${tableName(runtime, 'onboarding')} (user_id, created_at, updated_at)
         VALUES ($1, NOW(), NOW())
         ON CONFLICT (user_id) DO NOTHING`,
        [user.id],
      )
      return user
    },
    async createSession(
      sessionId,
      userID,
      expiresAt,
    ): Promise<void> {
      await db.query(
        `INSERT INTO ${tableName(runtime, 'sessions')} (id, user_id, created_at, expires_at)
         VALUES ($1, $2, NOW(), $3)`,
        [sessionId, userID, expiresAt],
      )
    },
    async deleteSession(sessionId): Promise<void> {
      await db.query(`DELETE FROM ${tableName(runtime, 'sessions')} WHERE id = $1`, [
        sessionId,
      ])
    },
    async getSession(sessionId, now): Promise<DashboardSessionRecord | null> {
      const result = await db.query<{
        user_id: string
        subject: string
        email: string
      }>(
        `SELECT s.user_id, u.subject, u.email
           FROM ${tableName(runtime, 'sessions')} s
           JOIN ${tableName(runtime, 'users')} u ON u.id = s.user_id
          WHERE s.id = $1 AND s.expires_at > $2`,
        [sessionId, now],
      )
      if (result.rowCount !== 1) {
        return null
      }
      const row = result.rows[0]
      return {
        sessionId,
        user: {
          id: row.user_id,
          subject: row.subject,
          email: row.email,
        },
      }
    },
  }
}

function createPlatformGateway(runtime: RuntimeConfig): PlatformGateway {
  return {
    async ensurePrincipal(user): Promise<void> {
      await unaryCall(runtime, 'EnsurePrincipal', {
        subject: user.subject,
        email: user.email,
      })
    },
    async listProjects(user): Promise<Array<DashboardProject>> {
      const response = await unaryCall(runtime, 'ListProjects', {}, user)
      const projects = Array.isArray(response.projects) ? response.projects : []
      return projects.map(toProject)
    },
    async createProject(user, name): Promise<DashboardProject> {
      const response = await unaryCall(runtime, 'CreateProject', { name }, user)
      return toProject(response)
    },
  }
}

async function ensureInitialized(): Promise<void> {
  if (!initPromise) {
    initPromise = initializeDatabase()
  }
  await initPromise
}

async function initializeDatabase(): Promise<void> {
  await pool.query(`CREATE SCHEMA IF NOT EXISTS ${config.databaseSchema}`)
  await pool.query(
    `CREATE TABLE IF NOT EXISTS ${tableName(config, 'users')} (
      id STRING PRIMARY KEY,
      subject STRING NOT NULL UNIQUE,
      email STRING NOT NULL,
      created_at TIMESTAMPTZ NOT NULL,
      updated_at TIMESTAMPTZ NOT NULL
    )`,
  )
  await pool.query(
    `CREATE TABLE IF NOT EXISTS ${tableName(config, 'sessions')} (
      id STRING PRIMARY KEY,
      user_id STRING NOT NULL REFERENCES ${tableName(config, 'users')}(id) ON DELETE CASCADE,
      created_at TIMESTAMPTZ NOT NULL,
      expires_at TIMESTAMPTZ NOT NULL
    )`,
  )
  await pool.query(
    `CREATE TABLE IF NOT EXISTS ${tableName(config, 'accounts')} (
      id STRING PRIMARY KEY,
      user_id STRING NOT NULL REFERENCES ${tableName(config, 'users')}(id) ON DELETE CASCADE,
      provider STRING NOT NULL,
      provider_subject STRING NOT NULL,
      created_at TIMESTAMPTZ NOT NULL,
      UNIQUE(provider, provider_subject)
    )`,
  )
  await pool.query(
    `CREATE TABLE IF NOT EXISTS ${tableName(config, 'onboarding')} (
      user_id STRING PRIMARY KEY REFERENCES ${tableName(config, 'users')}(id) ON DELETE CASCADE,
      account_name STRING NOT NULL DEFAULT '',
      status STRING NOT NULL DEFAULT 'pending',
      created_at TIMESTAMPTZ NOT NULL,
      updated_at TIMESTAMPTZ NOT NULL
    )`,
  )
  await pool.query(
    `CREATE INDEX IF NOT EXISTS ${config.databaseSchema}_sessions_expires_at_idx
        ON ${tableName(config, 'sessions')} (expires_at)`,
  )
}

async function unaryCall(
  runtime: RuntimeConfig,
  method: 'EnsurePrincipal' | 'ListProjects' | 'CreateProject',
  request: Record<string, unknown>,
  user?: DashboardUser,
): Promise<Record<string, unknown>> {
  const client = getPlatformClient(runtime)
  const metadata = new grpc.Metadata()
  if (user) {
    metadata.set('x-platform-user-subject', user.subject)
    metadata.set('x-platform-user-email', user.email)
  }
  return new Promise((resolve, reject) => {
    client[method](request, metadata, (error, response) => {
      if (error) {
        reject(error)
        return
      }
      resolve(response)
    })
  })
}

function getPlatformClient(runtime: RuntimeConfig): PlatformClient {
  if (clientInstance) {
    return clientInstance
  }
  const protoPath = resolve(process.cwd(), '../api/proto/platform.proto')
  const definition = protoLoader.loadSync(protoPath, {
    longs: String,
    enums: String,
    defaults: true,
    oneofs: true,
    includeDirs: [
      resolve(process.cwd(), '../api/proto'),
      dirname(googleProtoFiles.getProtoPath()),
    ],
  })
  const loaded = grpc.loadPackageDefinition(definition) as {
    platform: {
      v1: {
        PlatformService: grpc.ServiceClientConstructor
      }
    }
  }
  const credentials = grpc.credentials.createSsl(
    runtime.controlPlaneCA,
    runtime.controlPlaneKey,
    runtime.controlPlaneCert,
  )
  clientInstance = new loaded.platform.v1.PlatformService(
    runtime.controlPlaneAddress,
    grpc.credentials.combineChannelCredentials(
      credentials,
      grpc.credentials.createFromMetadataGenerator((_params, callback) => {
        callback(null, new grpc.Metadata())
      }),
    ),
    {
      'grpc.ssl_target_name_override': runtime.controlPlaneServerName,
      'grpc.default_authority': runtime.controlPlaneServerName,
    },
  ) as unknown as PlatformClient
  return clientInstance
}

function tableName(runtime: RuntimeConfig, name: string): string {
  return `${runtime.databaseSchema}.${name}`
}

function toProject(raw: Record<string, unknown>): DashboardProject {
  return {
    id: readString(raw.id),
    name: readString(raw.name),
    kind: readString(raw.kind),
    systemKey: emptyToUndefined(readString(raw.systemKey)),
  }
}

function readConfig(): RuntimeConfig {
  const databaseURL = mustEnv('DASHBOARD_DATABASE_URL')
  const databaseSchema = parseIdentifier(
    process.env.DASHBOARD_DATABASE_SCHEMA ?? 'dashboard',
  )
  const sessionCookieName =
    process.env.DASHBOARD_SESSION_COOKIE_NAME ?? 'dashboard_session'
  const publicBaseURL =
    process.env.DASHBOARD_PUBLIC_BASE_URL ?? 'http://localhost:3000'
  return {
    databaseURL,
    databaseSchema,
    sessionCookieName,
    publicBaseURL,
    controlPlaneAddress: mustEnv('DASHBOARD_CONTROLPLANE_ADDRESS'),
    controlPlaneServerName:
      process.env.DASHBOARD_CONTROLPLANE_SERVER_NAME ?? 'controlplane',
    controlPlaneCA: decodeBase64Env('DASHBOARD_CONTROLPLANE_CA_PEM_B64'),
    controlPlaneCert: decodeBase64Env('DASHBOARD_CONTROLPLANE_CERT_PEM_B64'),
    controlPlaneKey: decodeBase64Env('DASHBOARD_CONTROLPLANE_KEY_PEM_B64'),
    devUsers: parseDevUsers(process.env.DASHBOARD_DEV_USERS ?? ''),
    sessionMaxAgeSeconds: 7 * 24 * 60 * 60,
  }
}

function decodeBase64Env(name: string): Buffer {
  return Buffer.from(mustEnv(name), 'base64')
}

function mustEnv(name: string): string {
  const value = process.env[name]?.trim()
  if (!value) {
    throw new Error(`missing required environment variable ${name}`)
  }
  return value
}

function readString(value: unknown): string {
  return typeof value === 'string' ? value : ''
}

function emptyToUndefined(value: string): string | undefined {
  return value === '' ? undefined : value
}

export type {
  DashboardConfig,
  DashboardHomeState,
  DashboardProject,
  DashboardService,
  DashboardStore,
  DashboardUser,
  DevLoginIdentity,
  PlatformGateway,
  SessionCookies,
} from '#/lib/dashboard-core.server'
