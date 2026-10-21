-- name: CloseOpenInvitation :exec
-- Inviting an address again replaces its open invitation, so the old link stops working.
UPDATE project_invitations SET revoked_at = now()
WHERE project_id = $1 AND lower(email) = lower(sqlc.arg(email)) AND accepted_at IS NULL AND revoked_at IS NULL;

-- name: InsertInvitation :one
INSERT INTO project_invitations (project_id, email, role, token_hash, invited_by, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListOpenInvitations :many
-- Expired ones are listed too, so an admin sees who never came and can send a fresh link.
SELECT i.*, coalesce(u.name, '')::text AS invited_by_name
FROM project_invitations i LEFT JOIN users u ON u.id = i.invited_by
WHERE i.project_id = $1 AND i.accepted_at IS NULL AND i.revoked_at IS NULL
ORDER BY i.created_at DESC;

-- name: GetOpenInvitation :one
SELECT * FROM project_invitations
WHERE id = $1 AND project_id = $2 AND accepted_at IS NULL AND revoked_at IS NULL;

-- name: RenewInvitation :one
UPDATE project_invitations SET token_hash = $2, expires_at = $3
WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL
RETURNING *;

-- name: RevokeInvitation :execrows
UPDATE project_invitations SET revoked_at = now()
WHERE id = $1 AND project_id = $2 AND accepted_at IS NULL AND revoked_at IS NULL;

-- name: GetInvitationByTokenHash :one
SELECT * FROM project_invitations WHERE token_hash = $1;

-- name: HasOpenInvitationForEmail :one
SELECT EXISTS (
    SELECT 1 FROM project_invitations
    WHERE lower(email) = lower(sqlc.arg(email)) AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()
);

-- name: TakeInvitation :one
-- Marks one invitation accepted. Whoever takes it second finds nothing.
UPDATE project_invitations SET accepted_at = now(), accepted_by = sqlc.arg(user_id)
WHERE id = sqlc.arg(id) AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()
RETURNING *;

-- name: TakeInvitationsForEmail :many
UPDATE project_invitations SET accepted_at = now(), accepted_by = sqlc.arg(user_id)
WHERE lower(email) = lower(sqlc.arg(email)) AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()
RETURNING *;

-- name: JoinProject :exec
-- An invitation never takes a role away: a person already in the project keeps the higher of
-- the two.
INSERT INTO memberships (project_id, user_id, role) VALUES ($1, $2, $3)
ON CONFLICT (project_id, user_id) DO UPDATE SET role = CASE
    WHEN array_position(ARRAY['viewer', 'member', 'admin'], EXCLUDED.role) > array_position(ARRAY['viewer', 'member', 'admin'], memberships.role)
    THEN EXCLUDED.role ELSE memberships.role END;

-- name: LockProjectAdmins :many
-- Taken before changing anyone's role, so two admins demoting each other at once cannot
-- leave the project with none.
SELECT user_id FROM memberships WHERE project_id = $1 AND role = 'admin' FOR UPDATE;

-- name: DeleteMembership :execrows
DELETE FROM memberships WHERE project_id = $1 AND user_id = $2;

-- name: SearchProjectCandidates :many
-- People already in the workspace who are not in this project, by name or email. Only what
-- an admin needs to pick someone: no roles, no dates.
SELECT u.id, u.name, u.email FROM users u
WHERE (u.name ILIKE sqlc.arg(pattern) OR u.email ILIKE sqlc.arg(pattern))
  AND NOT EXISTS (SELECT 1 FROM memberships m WHERE m.project_id = sqlc.arg(project_id) AND m.user_id = u.id)
ORDER BY u.name
LIMIT 10;

-- name: IsProjectMemberByEmail :one
SELECT EXISTS (
    SELECT 1 FROM memberships m JOIN users u ON u.id = m.user_id
    WHERE m.project_id = $1 AND lower(u.email) = lower(sqlc.arg(email))
);
