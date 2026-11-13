import type { components } from './schema'

type Schemas = components['schemas']

export type AuthConfig = Schemas['AuthConfig']
export type Me = Schemas['Me']
export type User = Schemas['User']
export type Identity = Schemas['Identity']
export type TOTPSetup = Schemas['TOTPSetup']

export type Token = Schemas['Token']
export type CreatedToken = Schemas['CreatedToken']
export type CreateTokenRequest = Schemas['CreateTokenRequest']

export type Artifact = Schemas['Artifact']
export type ArtifactPatch = Schemas['UpdateArtifactRequest']
export type Version = Schemas['Version']
export type FileEntry = Schemas['FileEntry']
export type View = Schemas['View']
export type PreviewLink = Schemas['PreviewLink']

export type Invite = Schemas['Invite']
export type CreateInviteRequest = Schemas['CreateInviteRequest']
export type AdminUserPatch = Schemas['AdminUpdateUserRequest']
export type ResetLink = Schemas['ResetLink']
export type Stats = Schemas['Stats']

export type ArtifactStatus = 'active' | 'expired' | 'exhausted' | 'revoked'
export type ArtifactKind = 'html' | 'file' | 'bundle'
