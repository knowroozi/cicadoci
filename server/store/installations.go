// installations.go: CIC-146.
//
// Store methods for GitHub App installation events.  When GitHub
// delivers an installation or installation_repositories event, the
// handler updates repos.installation_id for the affected repos so that
// the clone minter can resolve installation IDs by repo without a
// separate lookup table.
package store

import (
	"context"
	"fmt"
)

// SetRepoInstallationIDByName updates repos.installation_id for a
// single repo, creating the org and repo rows if they don't exist yet.
//
// Used by HandleInstallationRepos to populate repos that haven't seen
// a push event yet.
func (s *Store) SetRepoInstallationIDByName(
	ctx context.Context, orgOwner, repoName, installationID string,
) error {
	orgID, err := s.UpsertOrg(ctx, orgOwner, orgOwner)
	if err != nil {
		return err
	}
	repoID, err := s.FindOrCreateRepo(ctx, orgID, "github", orgOwner, repoName, "main")
	if err != nil {
		return fmt.Errorf("store: SetRepoInstallationIDByName: %w", err)
	}
	_, err = s.pool.Exec(ctx,
		`UPDATE repos SET installation_id = $1, updated_at = now()
 WHERE id = $2`,
		installationID, repoID,
	)
	if err != nil {
		return fmt.Errorf("store: set installation_id for %s/%s: %w", orgOwner, repoName, err)
	}
	return nil
}
