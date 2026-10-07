package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"gha-runner-tui/internal/config"
)

type ScopeProof struct {
	Auth         AuthIdentity
	Repositories []config.RepoRef
	Complete     bool
	Digest       string
	ObservedAt   time.Time
}
type rawGroup struct {
	ID                       int64    `json:"id"`
	Name                     string   `json:"name"`
	Visibility               string   `json:"visibility"`
	AllowsPublicRepositories *bool    `json:"allows_public_repositories"`
	RestrictedToWorkflows    *bool    `json:"restricted_to_workflows"`
	SelectedWorkflows        []string `json:"selected_workflows"`
}
type rawRepository struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
	Visibility string `json:"visibility"`
	Private    *bool  `json:"private"`
}

func (r rawRepository) ref() config.RepoRef {
	return config.RepoRef{Owner: r.Owner.Login, Name: r.Name, ID: r.ID}
}
func (r rawRepository) known() bool {
	return r.ID > 0 && r.Name != "" && r.Owner.Login != "" && r.Private != nil && (r.Visibility == "private" && *r.Private || r.Visibility == "public" && !*r.Private || r.Visibility == "internal" && *r.Private)
}
func (c Client) EffectiveEligible(ctx context.Context, p config.Profile) (proof ScopeProof, err error) {
	proof = ScopeProof{Auth: c.Auth(), ObservedAt: c.now()}
	if err = c.checkValid(); err != nil {
		return proof, err
	}
	if c.transport == nil || p.GitHub.CredentialID != c.Auth().CredentialID || !strings.EqualFold(p.GitHub.ResourceOwner, c.transport.ref.ResourceOwner) {
		return proof, ErrIncomplete
	}
	if p.GitHub.TokenEnv != "" || p.GitHub.TokenFile != "" || p.GitHub.EnvFile != "" {
		if p.GitHub.TokenEnv != c.transport.ref.TokenEnv || p.GitHub.TokenFile != c.transport.ref.TokenFile || p.GitHub.EnvFile != c.transport.ref.EnvFile {
			return proof, ErrAuthChanged
		}
	}
	target, err := p.ResolveTarget()
	if err != nil {
		return proof, ErrIncomplete
	}
	var repos []rawRepository
	var group rawGroup
	if target.Scope == config.TargetScopeRepository {
		var r rawRepository
		if err = c.requestJSON(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s", target.Owner, target.Repo), nil, &r); err != nil {
			return proof, err
		}
		if RepoKey(r.ref()) != strings.ToLower(target.Owner+"/"+target.Repo) || !r.known() {
			return proof, ErrIncomplete
		}
		repos = []rawRepository{r}
	} else if target.Scope == config.TargetScopeOrganization {
		org := target.OrgSlug
		if !strings.EqualFold(org, c.transport.ref.ResourceOwner) {
			return proof, ErrIncomplete
		}
		groups, e := listPages[rawGroup](ctx, c, "/orgs/"+org+"/actions/runner-groups", "runner_groups", true)
		if e != nil {
			return proof, e
		}
		for _, g := range groups {
			if g.Name == p.RunnerGroup.Name {
				if group.ID != 0 {
					return proof, ErrIncomplete
				}
				group = g
			}
		}
		if group.ID <= 0 {
			return proof, ErrIncomplete
		}
		id := group.ID
		group = rawGroup{}
		if err = c.requestJSON(ctx, http.MethodGet, fmt.Sprintf("/orgs/%s/actions/runner-groups/%d", org, id), nil, &group); err != nil {
			return proof, err
		}
		if group.ID != id || group.Name != p.RunnerGroup.Name || group.AllowsPublicRepositories == nil || group.RestrictedToWorkflows == nil {
			return proof, ErrIncomplete
		}
		if *group.RestrictedToWorkflows {
			if len(group.SelectedWorkflows) == 0 {
				return proof, ErrIncomplete
			}
			for _, workflow := range group.SelectedWorkflows {
				proved := false
				for _, route := range p.Scheduler.Routes {
					if route.WorkflowRef == workflow && route.GroupID == group.ID && len(route.Labels) > 0 && route.EvidenceDigest != "" {
						proved = true
					}
				}
				if !proved {
					return proof, ErrIncomplete
				}
			}
		}
		switch group.Visibility {
		case "selected":
			repos, err = listPages[rawRepository](ctx, c, fmt.Sprintf("/orgs/%s/actions/runner-groups/%d/repositories", org, id), "repositories", true)
		case "all", "private":
			// Both REST lists are token-filtered. No protected, Auth-bound
			// qualification in this client establishes entire-org visibility.
			return proof, fmt.Errorf("scope authority unavailable for %s organization runner group: %w", group.Visibility, ErrIncomplete)
		default:
			return proof, ErrIncomplete
		}
		if err != nil {
			return proof, err
		}
		filtered := make([]rawRepository, 0, len(repos))
		for _, r := range repos {
			if !r.known() || !strings.EqualFold(r.Owner.Login, org) {
				return proof, ErrIncomplete
			}
			if r.Visibility == "public" && (group.Visibility == "private" || !*group.AllowsPublicRepositories) {
				continue
			}
			filtered = append(filtered, r)
		}
		repos = filtered
	} else {
		return proof, ErrIncomplete
	}
	seen := make(map[string]int64)
	for _, r := range repos {
		ref := r.ref()
		key := RepoKey(ref)
		if !r.known() || !c.repoAllowed(ref) {
			return proof, ErrIncomplete
		}
		covered := false
		for _, mapped := range p.Scheduler.Repositories {
			if RepoKey(mapped) == key && mapped.ID == ref.ID {
				covered = true
			}
		}
		if !covered {
			return proof, ErrIncomplete
		}
		if old, ok := seen[key]; ok {
			if old != ref.ID {
				return proof, ErrIncomplete
			}
			continue
		}
		seen[key] = ref.ID
		proof.Repositories = append(proof.Repositories, ref)
	}
	if len(proof.Repositories) == 0 {
		return proof, ErrIncomplete
	}
	if target.Scope == config.TargetScopeOrganization {
		for _, repo := range proof.Repositories {
			if err = c.verifyRepo(ctx, repo); err != nil {
				return proof, err
			}
		}
	}
	slices.SortFunc(proof.Repositories, func(a, b config.RepoRef) int { return strings.Compare(RepoKey(a), RepoKey(b)) })
	// Digest includes frozen identity, full access policy, and route evidence, not a token fingerprint.
	data, e := json.Marshal(struct {
		Auth   AuthIdentity
		Repos  []config.RepoRef
		Group  rawGroup
		Routes []config.RouteProof
		Target config.ResolvedTarget
	}{c.Auth(), proof.Repositories, group, p.Scheduler.Routes, target})
	if e != nil {
		return proof, ErrIncomplete
	}
	sum := sha256.Sum256(data)
	proof.Digest = hex.EncodeToString(sum[:])
	if err = c.checkValid(); err != nil {
		proof.Digest = ""
		proof.Repositories = nil
		return proof, err
	}
	proof.Complete = true
	return proof, nil
}
