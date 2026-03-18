import { Buffer } from 'node:buffer'
import { randomUUID } from 'node:crypto'
import { resolve } from 'node:path'
import * as grpc from '@grpc/grpc-js'
import * as protoLoader from '@grpc/proto-loader'
import { deleteCookie, getCookie, setCookie } from '@tanstack/react-start/server'
import googleProtoFiles from 'google-proto-files'
import { Pool } from 'pg'

export interface DashboardUser {
  id: string
  subject: string
  email: string
}

export interface DashboardProject {
  id: string
  name: string
  kind: string
  systemKey?: string
}

export interface DashboardHomeState {
  user: DashboardUser
  projects: Array<DashboardProject>
  controlPlaneReachable: boolean
  controlPlaneError?: string
}

export interface DevLoginIdentity {
  subject: string
  email: string
}

interface DashboardConfig {
  databaseURL: string
  databaseSchema: string
  sessionCookieName: string
  publicBaseURL: string
  controlPlaneAddress: string
  controlPlaneServerName: string
  controlPlaneCA: Buffer
  controlPlaneCert: Buffer
  controlPlaneKey: Buffer
  devUsers: Array<DevLoginIdentity>
  sessionMaxAgeSeconds: number
}

interface DashboardSession {
  sessionId: string
  user: DashboardUser
}

type PlatformClient = grpc.Client & {
  EnsurePrincipal: (
    request: Record<string, unknown>,
    metadata: grpc.Metadata,
    callback: (error: grpc.ServiceError | null, response: Record<string, unknown>) => void,
  ) => void
  ListProjects: (
    request: Record<string, unknown>,
    metadata: grpc.Metadata,
    callback: (error: grpc.ServiceError | null, response: Record<string, unknown>) => void,
  ) => void
  CreateProject: (
    request: Record<string, unknown>,
    metadata: grpc.Metadata,
    callback: (error: grpc.ServiceError | null, response: Record<string, unknown>) => void,
  ) => void
}

const config = readConfig()
const pool = new Pool({
  connectionString: config.databaseURL,
})

let initPromise: Promise<void> | undefined
let clientInstance: PlatformClient | undefined

export function listDevLogins(): Array<DevLoginIdentity> {
  return config.devUsers
}

export async function completeDevLogin(input: {
  subject: string
  email: string
  redirectTo?: string
}): Promise<string> {
  const subject = input.subject.trim()
  const email = input.email.trim()
  if (subject === '' || email === '') {
    throw new Error('subject and email are required')
  }
  await ensureInitialized()
  const user = await upsertUser(subject, email)
  const sessionId = randomUUID()
  const expiresAt = new Date(Date.now() + config.sessionMaxAgeSeconds * 1000)
  await pool.query(
    `INSERT INTO ${tableName('sessions')} (id, user_id, created_at, expires_at)
     VALUES ($1, $2, NOW(), $3)`,
    [sessionId, user.id, expiresAt],
  )
  setCookie(config.sessionCookieName, sessionId, sessionCookieOptions(expiresAt))
  try {
    await ensureProjectedPrincipal(user)
  } catch {
    // Keep the app session even if control-plane projection is temporarily unavailable.
  }
  return sanitizeRedirect(input.redirectTo)
}

export async function loadDashboardHome(): Promise<DashboardHomeState | null> {
  const session = await currentSession()
  if (!session) {
    return null
  }
  try {
    await ensureProjectedPrincipal(session.user)
    const projects = await listProjects(session.user)
    return {
      user: session.user,
      projects,
      controlPlaneReachable: true,
    }
  } catch (error) {
    return {
      user: session.user,
      projects: [],
      controlPlaneReachable: false,
      controlPlaneError: formatError(error),
    }
  }
}

export async function createProjectFromSession(name: string): Promise<DashboardProject> {
  const session = await requireSession()
  const projectName = name.trim()
  if (projectName === '') {
    throw new Error('project name is required')
  }
  await ensureProjectedPrincipal(session.user)
  const response = await unaryCall('CreateProject', { name: projectName }, session.user)
  return toProject(response)
}

export async function clearSession(): Promise<void> {
  const sessionId = getCookie(config.sessionCookieName)
  if (sessionId) {
    await ensureInitialized()
    await pool.query(`DELETE FROM ${tableName('sessions')} WHERE id = $1`, [sessionId])
  }
  deleteCookie(config.sessionCookieName, {
    path: '/',
  })
}

async function currentSession(): Promise<DashboardSession | null> {
  await ensureInitialized()
  const sessionId = getCookie(config.sessionCookieName)
  if (!sessionId) {
    return null
  }
  const result = await pool.query<{
    user_id: string
    subject: string
    email: string
  }>(
    `SELECT s.user_id, u.subject, u.email
       FROM ${tableName('sessions')} s
       JOIN ${tableName('users')} u ON u.id = s.user_id
      WHERE s.id = $1 AND s.expires_at > NOW()`,
    [sessionId],
  )
  if (result.rowCount !== 1) {
    await pool.query(`DELETE FROM ${tableName('sessions')} WHERE id = $1`, [sessionId])
    deleteCookie(config.sessionCookieName, { path: '/' })
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
}

async function requireSession(): Promise<DashboardSession> {
  const session = await currentSession()
  if (!session) {
    throw new Error('authentication required')
  }
  return session
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
    `CREATE TABLE IF NOT EXISTS ${tableName('users')} (
      id STRING PRIMARY KEY,
      subject STRING NOT NULL UNIQUE,
      email STRING NOT NULL,
      created_at TIMESTAMPTZ NOT NULL,
      updated_at TIMESTAMPTZ NOT NULL
    )`,
  )
  await pool.query(
    `CREATE TABLE IF NOT EXISTS ${tableName('sessions')} (
      id STRING PRIMARY KEY,
      user_id STRING NOT NULL REFERENCES ${tableName('users')}(id) ON DELETE CASCADE,
      created_at TIMESTAMPTZ NOT NULL,
      expires_at TIMESTAMPTZ NOT NULL
    )`,
  )
  await pool.query(
    `CREATE TABLE IF NOT EXISTS ${tableName('accounts')} (
      id STRING PRIMARY KEY,
      user_id STRING NOT NULL REFERENCES ${tableName('users')}(id) ON DELETE CASCADE,
      provider STRING NOT NULL,
      provider_subject STRING NOT NULL,
      created_at TIMESTAMPTZ NOT NULL,
      UNIQUE(provider, provider_subject)
    )`,
  )
  await pool.query(
    `CREATE TABLE IF NOT EXISTS ${tableName('onboarding')} (
      user_id STRING PRIMARY KEY REFERENCES ${tableName('users')}(id) ON DELETE CASCADE,
      account_name STRING NOT NULL DEFAULT '',
      status STRING NOT NULL DEFAULT 'pending',
      created_at TIMESTAMPTZ NOT NULL,
      updated_at TIMESTAMPTZ NOT NULL
    )`,
  )
  await pool.query(
    `CREATE INDEX IF NOT EXISTS ${config.databaseSchema}_sessions_expires_at_idx
        ON ${tableName('sessions')} (expires_at)`,
  )
}

async function upsertUser(subject: string, email: string): Promise<DashboardUser> {
  const id = randomUUID()
  const result = await pool.query<{
    id: string
    subject: string
    email: string
  }>(
    `INSERT INTO ${tableName('users')} (id, subject, email, created_at, updated_at)
     VALUES ($1, $2, $3, NOW(), NOW())
     ON CONFLICT (subject) DO UPDATE SET email = excluded.email, updated_at = NOW()
     RETURNING id, subject, email`,
    [id, subject, email],
  )
  const user = result.rows[0]
  await pool.query(
    `INSERT INTO ${tableName('accounts')} (id, user_id, provider, provider_subject, created_at)
     VALUES ($1, $2, $3, $4, NOW())
     ON CONFLICT (provider, provider_subject) DO NOTHING`,
    [randomUUID(), user.id, 'dev', user.subject],
  )
  await pool.query(
    `INSERT INTO ${tableName('onboarding')} (user_id, created_at, updated_at)
     VALUES ($1, NOW(), NOW())
     ON CONFLICT (user_id) DO NOTHING`,
    [user.id],
  )
  return user
}

async function ensureProjectedPrincipal(user: DashboardUser): Promise<void> {
  await unaryCall('EnsurePrincipal', {
    subject: user.subject,
    email: user.email,
  })
}

async function listProjects(user: DashboardUser): Promise<Array<DashboardProject>> {
  const response = await unaryCall('ListProjects', {}, user)
  const projects = Array.isArray(response.projects) ? response.projects : []
  return projects.map(toProject)
}

async function unaryCall(
  method: 'EnsurePrincipal' | 'ListProjects' | 'CreateProject',
  request: Record<string, unknown>,
  user?: DashboardUser,
): Promise<Record<string, unknown>> {
  const client = getPlatformClient()
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

function getPlatformClient(): PlatformClient {
  if (clientInstance) {
    return clientInstance
  }
  const protoPath = resolve(process.cwd(), '../api/proto/platform.proto')
  const definition = protoLoader.loadSync(protoPath, {
    longs: String,
    enums: String,
    defaults: true,
    oneofs: true,
    includeDirs: [resolve(process.cwd(), '../api/proto'), googleProtoFiles.getProtoPath()],
  })
  const loaded = grpc.loadPackageDefinition(definition) as {
    platform: {
      v1: {
        PlatformService: grpc.ServiceClientConstructor
      }
    }
  }
  const credentials = grpc.credentials.createSsl(
    config.controlPlaneCA,
    config.controlPlaneKey,
    config.controlPlaneCert,
  )
  clientInstance = new loaded.platform.v1.PlatformService(
    config.controlPlaneAddress,
    grpc.credentials.combineChannelCredentials(
      credentials,
      grpc.credentials.createFromMetadataGenerator((_params, callback) => {
        callback(null, new grpc.Metadata())
      }),
    ),
    {
      'grpc.ssl_target_name_override': config.controlPlaneServerName,
      'grpc.default_authority': config.controlPlaneServerName,
    },
  ) as unknown as PlatformClient
  return clientInstance
}

function tableName(name: string): string {
  return `${config.databaseSchema}.${name}`
}

function sessionCookieOptions(expiresAt: Date) {
  return {
    httpOnly: true,
    path: '/',
    sameSite: 'lax' as const,
    secure: config.publicBaseURL.startsWith('https://'),
    expires: expiresAt,
  }
}

function sanitizeRedirect(value?: string): string {
  if (!value || !value.startsWith('/')) {
    return '/'
  }
  return value
}

function toProject(raw: Record<string, unknown>): DashboardProject {
  return {
    id: readString(raw.id),
    name: readString(raw.name),
    kind: readString(raw.kind),
    systemKey: emptyToUndefined(readString(raw.systemKey)),
  }
}

function formatError(error: unknown): string {
  if (error && typeof error === 'object' && 'message' in error) {
    return String(error.message)
  }
  return 'unknown error'
}

function readConfig(): DashboardConfig {
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

function parseIdentifier(raw: string): string {
  if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(raw)) {
    throw new Error(`invalid dashboard schema identifier: ${raw}`)
  }
  return raw
}

function parseDevUsers(raw: string): Array<DevLoginIdentity> {
  if (raw.trim() === '') {
    return []
  }
  return raw
    .split(';')
    .map((entry) => entry.trim())
    .filter((entry) => entry !== '')
    .map((entry) => {
      const [subject, email] = entry.split(':', 2)
      return {
        subject: (subject ?? '').trim(),
        email: (email ?? '').trim(),
      }
    })
    .filter((entry) => entry.subject !== '' && entry.email !== '')
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
